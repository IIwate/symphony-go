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
	defaultNotificationQueueSize  = 256
	maxNotificationWorkerCount    = 4
	notificationDrainTimeout      = 5 * time.Second
	notificationQueueOverflowCode = "notification_queue_overflow"
)

type notifier interface {
	Emit(event model.RuntimeEvent)
	Close(ctx context.Context) error
}

type asyncNotifier struct {
	logger         *slog.Logger
	channels       []notificationChannel
	defaults       model.NotificationDefaultsConfig
	deliveryResult func(channel string, err error)
	enqueueResult  func(err error)
	client         *http.Client
	queue          chan model.RuntimeEvent

	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup
}

type notificationChannel struct {
	name    string
	kind    model.NotificationChannelKind
	url     string
	headers map[string]string
	events  map[model.NotificationEventType]struct{}
}

type webhookPayload struct {
	EventID            string  `json:"event_id"`
	Type               string  `json:"type"`
	Level              string  `json:"level"`
	OccurredAt         string  `json:"occurred_at"`
	IssueID            string  `json:"issue_id,omitempty"`
	Identifier         string  `json:"identifier,omitempty"`
	Message            string  `json:"message,omitempty"`
	State              string  `json:"state,omitempty"`
	RunPhase           string  `json:"run_phase,omitempty"`
	AttemptCount       int     `json:"attempt_count,omitempty"`
	WorkspacePath      string  `json:"workspace_path,omitempty"`
	DispatchKind       string  `json:"dispatch_kind,omitempty"`
	ExpectedOutcome    string  `json:"expected_outcome,omitempty"`
	ContinuationReason *string `json:"continuation_reason,omitempty"`
	Branch             string  `json:"branch,omitempty"`
	Reason             string  `json:"reason,omitempty"`
	PRNumber           int     `json:"pr_number,omitempty"`
	PRURL              string  `json:"pr_url,omitempty"`
	PRState            string  `json:"pr_state,omitempty"`
	AlertCode          string  `json:"alert_code,omitempty"`
	AlertLevel         string  `json:"alert_level,omitempty"`
	Error              string  `json:"error,omitempty"`
}

type slackPayload struct {
	Text string `json:"text"`
}

func newAsyncNotifier(cfg model.NotificationsConfig, logger *slog.Logger, deliveryResult func(channel string, err error), enqueueResult func(err error)) notifier {
	if len(cfg.Channels) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	channels := make([]notificationChannel, 0, len(cfg.Channels))
	for _, channel := range cfg.Channels {
		events := make(map[model.NotificationEventType]struct{}, len(channel.Events))
		for _, eventType := range channel.Events {
			events[eventType] = struct{}{}
		}
		headers := make(map[string]string, len(channel.Headers))
		for key, value := range channel.Headers {
			headers[key] = value
		}
		channels = append(channels, notificationChannel{
			name:    channel.Name,
			kind:    channel.Kind,
			url:     channel.URL,
			headers: headers,
			events:  events,
		})
	}

	queueSize := maxInt(defaultNotificationQueueSize, len(channels)*32)
	workerCount := len(channels)
	if workerCount <= 0 {
		workerCount = 1
	}
	if workerCount > maxNotificationWorkerCount {
		workerCount = maxNotificationWorkerCount
	}

	n := &asyncNotifier{
		logger:         logger,
		channels:       channels,
		defaults:       cfg.Defaults,
		deliveryResult: deliveryResult,
		enqueueResult:  enqueueResult,
		client: &http.Client{
			Timeout: time.Duration(cfg.Defaults.TimeoutMS) * time.Millisecond,
		},
		queue: make(chan model.RuntimeEvent, queueSize),
	}
	for workerID := 0; workerID < workerCount; workerID++ {
		n.wg.Add(1)
		go n.worker()
	}
	return n
}

func (n *asyncNotifier) Emit(event model.RuntimeEvent) {
	if n == nil {
		return
	}

	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		if n.enqueueResult != nil {
			go n.enqueueResult(fmt.Errorf("notification notifier is closed"))
		}
		return
	}

	select {
	case n.queue <- event:
	default:
		if n.enqueueResult != nil {
			go n.enqueueResult(fmt.Errorf("notification queue is full"))
		}
		n.logger.Warn("notification queue is full", "event_type", event.Type, "event_id", event.EventID)
	}
}

func (n *asyncNotifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	n.mu.Lock()
	if !n.closed {
		n.closed = true
		close(n.queue)
	}
	n.mu.Unlock()

	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *asyncNotifier) worker() {
	defer n.wg.Done()
	for event := range n.queue {
		n.deliverEvent(event)
		if n.enqueueResult != nil {
			n.enqueueResult(nil)
		}
	}
}

func (n *asyncNotifier) deliverEvent(event model.RuntimeEvent) {
	for _, channel := range n.channels {
		if len(channel.events) > 0 {
			if _, ok := channel.events[event.Type]; !ok {
				continue
			}
		}
		err := n.deliver(channel, event)
		if n.deliveryResult != nil {
			n.deliveryResult(channel.name, err)
		}
	}
}

func (n *asyncNotifier) deliver(channel notificationChannel, event model.RuntimeEvent) error {
	payload, contentType, err := n.encode(channel.kind, event)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt <= n.defaults.RetryCount; attempt++ {
		req, err := http.NewRequest(http.MethodPost, channel.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		for key, value := range channel.headers {
			req.Header.Set(key, value)
		}

		resp, err := n.client.Do(req)
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

		if attempt >= n.defaults.RetryCount {
			break
		}
		time.Sleep(time.Duration(n.defaults.RetryDelayMS) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("notification delivery failed")
	}
	return lastErr
}

func (n *asyncNotifier) encode(kind model.NotificationChannelKind, event model.RuntimeEvent) ([]byte, string, error) {
	switch kind {
	case model.NotificationChannelKindSlack:
		raw, err := json.Marshal(slackPayload{Text: renderSlackText(event)})
		return raw, "application/json", err
	case model.NotificationChannelKindWebhook:
		raw, err := json.Marshal(webhookPayload{
			EventID:            event.EventID,
			Type:               string(event.Type),
			Level:              event.Level,
			OccurredAt:         event.OccurredAt.UTC().Format(time.RFC3339),
			IssueID:            event.IssueID,
			Identifier:         event.Identifier,
			Message:            event.Message,
			State:              event.State,
			RunPhase:           event.RunPhase,
			AttemptCount:       event.AttemptCount,
			WorkspacePath:      event.WorkspacePath,
			DispatchKind:       event.DispatchKind,
			ExpectedOutcome:    event.ExpectedOutcome,
			ContinuationReason: cloneReason(event.ContinuationReason),
			Branch:             event.Branch,
			Reason:             event.Reason,
			PRNumber:           event.PRNumber,
			PRURL:              event.PRURL,
			PRState:            event.PRState,
			AlertCode:          event.AlertCode,
			AlertLevel:         event.AlertLevel,
			Error:              event.Error,
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
	next := newAsyncNotifier(o.currentConfig().Notifications, o.logger, o.handleNotificationDeliveryResult, o.handleNotificationEnqueueResult)
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

func (o *Orchestrator) handleNotificationEnqueueResult(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if err == nil {
		if o.clearSystemAlertLocked(notificationQueueOverflowCode) {
			o.commitStateLocked(false)
		}
		return
	}

	if !o.setSystemAlertLocked(AlertSnapshot{
		Code:    notificationQueueOverflowCode,
		Level:   "warn",
		Message: err.Error(),
	}) {
		return
	}
	o.commitStateLocked(false)
}

func (o *Orchestrator) handleNotificationDeliveryResult(channel string, err error) {
	code := "notification_delivery_failed_" + model.SanitizeWorkspaceKey(channel)

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

func shouldSuppressAlertNotification(code string) bool {
	return strings.HasPrefix(code, "notification_")
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

	details := make(map[string]string)
	appendDetail := func(key string, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		details[key] = value
	}

	appendDetail("state", event.State)
	appendDetail("run_phase", event.RunPhase)
	if event.AttemptCount > 0 {
		details["attempt_count"] = fmt.Sprintf("%d", event.AttemptCount)
	}
	appendDetail("workspace_path", event.WorkspacePath)
	appendDetail("dispatch_kind", event.DispatchKind)
	appendDetail("expected_outcome", event.ExpectedOutcome)
	if event.ContinuationReason != nil {
		appendDetail("continuation_reason", *event.ContinuationReason)
	}
	appendDetail("branch", event.Branch)
	appendDetail("reason", event.Reason)
	if event.PRNumber > 0 {
		details["pr_number"] = fmt.Sprintf("%d", event.PRNumber)
	}
	appendDetail("pr_url", event.PRURL)
	appendDetail("pr_state", event.PRState)
	appendDetail("alert_code", event.AlertCode)
	appendDetail("alert_level", event.AlertLevel)
	appendDetail("error", event.Error)

	if len(details) == 0 {
		return strings.Join(lines, "\n")
	}

	keys := make([]string, 0, len(details))
	for key := range details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lines = append(lines, fmt.Sprintf("%s: %s", key, details[key]))
	}
	return strings.Join(lines, "\n")
}

func cloneReason(value *string) *string {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func (o *Orchestrator) newIssueDispatchedEvent(issue model.Issue, attempt int, dispatch *model.DispatchContext) model.RuntimeEvent {
	event := model.RuntimeEvent{
		EventID:      o.nextEventID(),
		Type:         model.NotificationEventIssueDispatched,
		Level:        "info",
		OccurredAt:   o.now().UTC(),
		IssueID:      issue.ID,
		Identifier:   issue.Identifier,
		Message:      fmt.Sprintf("issue %s dispatched", issue.Identifier),
		State:        issue.State,
		AttemptCount: attemptCountFromRetry(attempt),
	}
	if dispatch != nil {
		event.DispatchKind = string(dispatch.Kind)
		event.ExpectedOutcome = string(dispatch.ExpectedOutcome)
		event.ContinuationReason = cloneContinuationReason(dispatch)
	}
	return event
}

func (o *Orchestrator) newIssueFailedEvent(issueID string, identifier string, workspacePath string, phase model.RunPhase, attempt int, err error, dispatch *model.DispatchContext) model.RuntimeEvent {
	event := model.RuntimeEvent{
		EventID:       o.nextEventID(),
		Type:          model.NotificationEventIssueFailed,
		Level:         "warn",
		OccurredAt:    o.now().UTC(),
		IssueID:       issueID,
		Identifier:    identifier,
		Message:       fmt.Sprintf("issue %s failed", identifier),
		RunPhase:      phase.String(),
		AttemptCount:  attemptCountFromRetry(attempt),
		WorkspacePath: workspacePath,
		Error:         errorString(err),
	}
	if dispatch != nil {
		event.DispatchKind = string(dispatch.Kind)
		event.ExpectedOutcome = string(dispatch.ExpectedOutcome)
		event.ContinuationReason = cloneContinuationReason(dispatch)
	}
	return event
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
	event := model.RuntimeEvent{
		EventID:         o.nextEventID(),
		Type:            model.NotificationEventIssueInterventionRequired,
		Level:           "warn",
		OccurredAt:      o.now().UTC(),
		IssueID:         issueID,
		Identifier:      identifier,
		Message:         fmt.Sprintf("issue %s requires manual intervention", identifier),
		Branch:          branch,
		Reason:          reason,
		ExpectedOutcome: string(expectedOutcome),
	}
	if pr != nil {
		event.PRNumber = pr.Number
		event.PRURL = pr.URL
		event.PRState = string(pr.State)
	}
	return event
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
		AlertCode:  alert.Code,
		AlertLevel: alert.Level,
	}
}

func cloneContinuationReason(dispatch *model.DispatchContext) *string {
	if dispatch == nil || dispatch.Reason == nil {
		return nil
	}
	reason := string(*dispatch.Reason)
	return &reason
}
