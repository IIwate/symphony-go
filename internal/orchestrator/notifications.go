package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"symphony-go/internal/model"
)

const (
	defaultNotificationQueueSize   = 128
	notificationDrainTimeout       = 5 * time.Second
	notificationQueueOverflowPrefix = "notification_queue_overflow_"
	notificationDeliveryFailPrefix  = "notification_delivery_failed_"
)

type notifier interface {
	Emit(event model.RuntimeEvent)
	Close(ctx context.Context) error
}

type asyncNotifier struct {
	logger         *slog.Logger
	emitters       []*notificationEmitter
	enqueueResult  func(channel string, err error)
}

type notificationEmitter struct {
	logger         *slog.Logger
	channel        notificationChannel
	client         *http.Client
	queue          chan model.RuntimeEvent
	deliveryResult func(channel string, err error)

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

type notificationChannel struct {
	name         string
	kind         model.NotificationChannelKind
	url          string
	headers      map[string]string
	subscribeAll bool
	events       map[model.NotificationEventType]struct{}
}

type webhookPayload struct {
	EventID    string         `json:"event_id"`
	Type       string         `json:"type"`
	Level      string         `json:"level"`
	OccurredAt string         `json:"occurred_at"`
	IssueID    string         `json:"issue_id,omitempty"`
	Identifier string         `json:"identifier,omitempty"`
	Message    string         `json:"message,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

type slackPayload struct {
	Text string `json:"text"`
}

func newAsyncNotifier(
	cfg model.NotificationsConfig,
	logger *slog.Logger,
	deliveryResult func(channel string, err error),
	enqueueResult func(channel string, err error),
) notifier {
	if len(cfg.Channels) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	emitters := make([]*notificationEmitter, 0, len(cfg.Channels))
	for _, channelCfg := range cfg.Channels {
		subscribeAll := false
		events := make(map[model.NotificationEventType]struct{}, len(channelCfg.Events))
		for _, eventType := range channelCfg.Events {
			if eventType == model.NotificationEventAll {
				subscribeAll = true
				continue
			}
			events[eventType] = struct{}{}
		}
		headers := make(map[string]string, len(channelCfg.Headers))
		for key, value := range channelCfg.Headers {
			headers[key] = value
		}

		emitter := &notificationEmitter{
			logger: logger,
			channel: notificationChannel{
				name:         channelCfg.Name,
				kind:         channelCfg.Kind,
				url:          channelCfg.URL,
				headers:      headers,
				subscribeAll: subscribeAll,
				events:       events,
			},
			client: &http.Client{
				Timeout: time.Duration(cfg.Defaults.TimeoutMS) * time.Millisecond,
			},
			queue:          make(chan model.RuntimeEvent, defaultNotificationQueueSize),
			deliveryResult: deliveryResult,
		}
		emitter.wg.Add(1)
		go emitter.worker(cfg.Defaults)
		emitters = append(emitters, emitter)
	}

	return &asyncNotifier{
		logger:        logger,
		emitters:      emitters,
		enqueueResult: enqueueResult,
	}
}

func (n *asyncNotifier) Emit(event model.RuntimeEvent) {
	if n == nil {
		return
	}
	for _, emitter := range n.emitters {
		if !emitter.subscribed(event.Type) {
			continue
		}
		err := emitter.emit(event)
		if n.enqueueResult != nil {
			n.enqueueResult(emitter.channel.name, err)
		}
		if err != nil {
			n.logger.Warn("notification queue is full", "channel", emitter.channel.name, "event_type", event.Type, "event_id", event.EventID)
		}
	}
}

func (n *asyncNotifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var firstErr error
	for _, emitter := range n.emitters {
		if err := emitter.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (e *notificationEmitter) subscribed(eventType model.NotificationEventType) bool {
	if e == nil {
		return false
	}
	if e.channel.subscribeAll {
		return true
	}
	_, ok := e.channel.events[eventType]
	return ok
}

func (e *notificationEmitter) emit(event model.RuntimeEvent) error {
	if e == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("notification notifier is closed")
	}

	select {
	case e.queue <- event:
		return nil
	default:
		return fmt.Errorf("notification queue is full")
	}
}

func (e *notificationEmitter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	e.mu.Lock()
	if !e.closed {
		e.closed = true
		close(e.queue)
	}
	e.mu.Unlock()

	done := make(chan struct{})
	go func() {
		e.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *notificationEmitter) worker(defaults model.NotificationDefaultsConfig) {
	defer e.wg.Done()
	for event := range e.queue {
		err := e.deliver(defaults, event)
		if e.deliveryResult != nil {
			e.deliveryResult(e.channel.name, err)
		}
	}
}

func (e *notificationEmitter) deliver(defaults model.NotificationDefaultsConfig, event model.RuntimeEvent) error {
	payload, contentType, err := encodeNotificationEvent(e.channel.kind, event)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt <= defaults.RetryCount; attempt++ {
		req, err := http.NewRequest(http.MethodPost, e.channel.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		for key, value := range e.channel.headers {
			req.Header.Set(key, value)
		}

		resp, err := e.client.Do(req)
		if err == nil && resp != nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				_ = resp.Body.Close()
				return nil
			}
			lastErr = fmt.Errorf("unexpected notification status %d", resp.StatusCode)
			_ = resp.Body.Close()
		} else {
			lastErr = err
		}

		if attempt >= defaults.RetryCount {
			break
		}
		time.Sleep(time.Duration(defaults.RetryDelayMS) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("notification delivery failed")
	}
	return lastErr
}

func encodeNotificationEvent(kind model.NotificationChannelKind, event model.RuntimeEvent) ([]byte, string, error) {
	switch kind {
	case model.NotificationChannelKindSlack:
		raw, err := json.Marshal(slackPayload{Text: renderSlackText(event)})
		return raw, "application/json", err
	case model.NotificationChannelKindWebhook:
		raw, err := json.Marshal(webhookPayload{
			EventID:    event.EventID,
			Type:       string(event.Type),
			Level:      event.Level,
			OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339),
			IssueID:    event.IssueID,
			Identifier: event.Identifier,
			Message:    event.Message,
			Details:    cloneDetails(event.Details),
		})
		return raw, "application/json", err
	default:
		return nil, "", fmt.Errorf("unsupported notification channel kind %q", kind)
	}
}

func (o *Orchestrator) nextEventID() string {
	sequence := atomic.AddUint64(&o.eventSeq, 1)
	return fmt.Sprintf("evt-%d-%06d", o.now().UTC().UnixNano(), sequence)
}

func (o *Orchestrator) reloadNotifierLocked() notifier {
	next := newAsyncNotifier(o.currentConfig().Notifications, o.logger, o.reportNotificationDeliveryResult, o.reportNotificationEnqueueResult)
	previous := o.notifier
	o.notifier = next
	return previous
}

func (o *Orchestrator) closeNotifier(active notifier) {
	if active == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), notificationDrainTimeout)
	defer cancel()
	if err := active.Close(ctx); err != nil {
		o.logger.Warn("notification drain timed out", "error", err.Error())
	}
}

func (o *Orchestrator) emitNotificationLocked(event model.RuntimeEvent) {
	if o.notifier == nil {
		return
	}
	o.notifier.Emit(event)
}

func notificationQueueOverflowCode(channel string) string {
	return notificationQueueOverflowPrefix + model.SanitizeWorkspaceKey(channel)
}

func notificationDeliveryFailedCode(channel string) string {
	return notificationDeliveryFailPrefix + model.SanitizeWorkspaceKey(channel)
}

func (o *Orchestrator) reportNotificationEnqueueResult(channel string, err error) {
	o.postRuntimeExtensionEvent(runtimeExtensionEvent{
		Kind:    runtimeExtensionEventNotificationEnqueue,
		Channel: channel,
		Err:     err,
	})
}

func (o *Orchestrator) reportNotificationDeliveryResult(channel string, err error) {
	o.postRuntimeExtensionEvent(runtimeExtensionEvent{
		Kind:    runtimeExtensionEventNotificationDeliver,
		Channel: channel,
		Err:     err,
	})
}

func (o *Orchestrator) handleNotificationEnqueueResult(channel string, err error) {
	code := notificationQueueOverflowCode(channel)

	o.mu.Lock()
	defer o.mu.Unlock()

	if err == nil {
		if o.clearSystemAlertLocked(code) {
			o.commitStateLocked(false)
		}
		return
	}

	if !o.setSystemAlertLocked(AlertSnapshot{
		Code:    code,
		Level:   "warn",
		Message: err.Error(),
	}) {
		return
	}
	o.commitStateLocked(false)
}

func (o *Orchestrator) handleNotificationDeliveryResult(channel string, err error) {
	code := notificationDeliveryFailedCode(channel)

	o.mu.Lock()
	defer o.mu.Unlock()

	if err == nil {
		if o.clearSystemAlertLocked(code) {
			o.commitStateLocked(false)
		}
		return
	}

	o.logger.Warn("notification delivery failed", "channel", channel, "error", err.Error())
	if !o.setSystemAlertLocked(AlertSnapshot{
		Code:    code,
		Level:   "warn",
		Message: fmt.Sprintf("notification channel %s failed: %s", channel, err.Error()),
	}) {
		return
	}
	o.commitStateLocked(false)
}

func renderSlackText(event model.RuntimeEvent) string {
	lines := []string{
		fmt.Sprintf("[%s] %s", strings.ToUpper(strings.TrimSpace(event.Level)), string(event.Type)),
	}
	if strings.TrimSpace(event.Message) != "" {
		lines = append(lines, event.Message)
	}
	if strings.TrimSpace(event.Identifier) != "" {
		lines = append(lines, fmt.Sprintf("Issue: %s", event.Identifier))
	} else if strings.TrimSpace(event.IssueID) != "" {
		lines = append(lines, fmt.Sprintf("IssueID: %s", event.IssueID))
	}

	if len(event.Details) == 0 {
		return strings.Join(lines, "\n")
	}

	keys := make([]string, 0, len(event.Details))
	for key := range event.Details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := detailString(event.Details[key])
		if strings.TrimSpace(value) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", key, value))
	}
	return strings.Join(lines, "\n")
}

func cloneDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	copyDetails := make(map[string]any, len(details))
	for key, value := range details {
		copyDetails[key] = value
	}
	return copyDetails
}

func detailString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case *string:
		if typed == nil {
			return ""
		}
		return *typed
	default:
		return fmt.Sprint(value)
	}
}

func eventDetails(pairs ...any) map[string]any {
	if len(pairs) == 0 {
		return nil
	}
	details := make(map[string]any, len(pairs)/2)
	for index := 0; index+1 < len(pairs); index += 2 {
		key, ok := pairs[index].(string)
		if !ok || strings.TrimSpace(key) == "" {
			continue
		}
		value := pairs[index+1]
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if strings.TrimSpace(typed) == "" {
				continue
			}
		case *string:
			if typed == nil || strings.TrimSpace(*typed) == "" {
				continue
			}
			value = *typed
		case int:
			if typed == 0 {
				continue
			}
		}
		details[key] = value
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func (o *Orchestrator) newIssueDispatchedEvent(issue model.Issue, attempt int, dispatch *model.DispatchContext) model.RuntimeEvent {
	details := eventDetails(
		"state", issue.State,
		"attempt_count", attemptCountFromRetry(attempt),
	)
	if dispatch != nil {
		maps := eventDetails(
			"dispatch_kind", string(dispatch.Kind),
			"expected_outcome", string(dispatch.ExpectedOutcome),
			"continuation_reason", cloneContinuationReason(dispatch),
		)
		for key, value := range maps {
			if details == nil {
				details = map[string]any{}
			}
			details[key] = value
		}
	}
	return model.RuntimeEvent{
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueDispatched,
		Level:      "info",
		OccurredAt: o.now().UTC(),
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Message:    fmt.Sprintf("issue %s dispatched", issue.Identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newIssueFailedEvent(issueID string, identifier string, workspacePath string, phase model.RunPhase, attempt int, err error, dispatch *model.DispatchContext) model.RuntimeEvent {
	details := eventDetails(
		"run_phase", phase.String(),
		"attempt_count", attemptCountFromRetry(attempt),
		"workspace_path", workspacePath,
		"error", errorString(err),
	)
	if dispatch != nil {
		maps := eventDetails(
			"dispatch_kind", string(dispatch.Kind),
			"expected_outcome", string(dispatch.ExpectedOutcome),
			"continuation_reason", cloneContinuationReason(dispatch),
		)
		for key, value := range maps {
			if details == nil {
				details = map[string]any{}
			}
			details[key] = value
		}
	}
	return model.RuntimeEvent{
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueFailed,
		Level:      "warn",
		OccurredAt: o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s failed", identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newIssueCompletedEvent(issueID string, identifier string) model.RuntimeEvent {
	return model.RuntimeEvent{
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueCompleted,
		Level:      "info",
		OccurredAt: o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s completed", identifier),
	}
}

func (o *Orchestrator) newIssueInterventionRequiredEvent(issueID string, identifier string, branch string, reason string, expectedOutcome model.CompletionMode, pr *PullRequestInfo) model.RuntimeEvent {
	details := eventDetails(
		"branch", branch,
		"reason", reason,
		"expected_outcome", string(expectedOutcome),
	)
	if pr != nil {
		maps := eventDetails(
			"pr_number", pr.Number,
			"pr_url", pr.URL,
			"pr_state", string(pr.State),
		)
		for key, value := range maps {
			if details == nil {
				details = map[string]any{}
			}
			details[key] = value
		}
	}
	return model.RuntimeEvent{
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueInterventionRequired,
		Level:      "warn",
		OccurredAt: o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s requires manual intervention", identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newSystemAlertEvent(eventType model.NotificationEventType, alert AlertSnapshot) model.RuntimeEvent {
	return model.RuntimeEvent{
		EventID:    o.nextEventID(),
		Type:       eventType,
		Level:      alert.Level,
		OccurredAt: o.now().UTC(),
		IssueID:    alert.IssueID,
		Identifier: alert.IssueIdentifier,
		Message:    alert.Message,
		Details: eventDetails(
			"alert_code", alert.Code,
			"alert_level", alert.Level,
		),
	}
}

func cloneContinuationReason(dispatch *model.DispatchContext) *string {
	if dispatch == nil || dispatch.Reason == nil {
		return nil
	}
	reason := string(*dispatch.Reason)
	return &reason
}
