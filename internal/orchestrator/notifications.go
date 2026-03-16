package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"symphony-go/internal/model"
)

const (
	notificationDrainTimeout        = 5 * time.Second
	notificationQueueOverflowPrefix = "notification_queue_overflow_"
	notificationDeliveryFailPrefix  = "notification_delivery_failed_"
	runtimeEventVersion             = 1
)

type notifier interface {
	Emit(event model.RuntimeEvent)
	Close(ctx context.Context) error
}

type asyncNotifier struct {
	logger        *slog.Logger
	emitters      []*notificationEmitter
	enqueueResult func(generation uint64, channelID string, err error)
}

type notificationEmitter struct {
	logger         *slog.Logger
	generation     uint64
	channel        notificationChannel
	normalQueue    chan model.RuntimeEvent
	criticalQueue  chan model.RuntimeEvent
	deliveryResult func(generation uint64, channelID string, err error)

	mu             sync.Mutex
	closed         bool
	overflowActive bool
	wg             sync.WaitGroup
}

type notificationChannel struct {
	id          string
	displayName string
	kind        model.NotificationChannelKind
	families    map[model.RuntimeEventFamily]struct{}
	types       map[model.NotificationEventType]struct{}
	delivery    model.NotificationDeliveryConfig
	webhook     *model.WebhookNotificationConfig
	slack       *model.SlackNotificationConfig
}

func newAsyncNotifier(
	cfg model.NotificationsConfig,
	logger *slog.Logger,
	generation uint64,
	deliveryResult func(generation uint64, channelID string, err error),
	enqueueResult func(generation uint64, channelID string, err error),
) notifier {
	if len(cfg.Channels) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}

	emitters := make([]*notificationEmitter, 0, len(cfg.Channels))
	for _, channelCfg := range cfg.Channels {
		families := make(map[model.RuntimeEventFamily]struct{}, len(channelCfg.Subscriptions.Families))
		for _, family := range channelCfg.Subscriptions.Families {
			families[family] = struct{}{}
		}
		types := make(map[model.NotificationEventType]struct{}, len(channelCfg.Subscriptions.Types))
		for _, eventType := range channelCfg.Subscriptions.Types {
			types[eventType] = struct{}{}
		}

		emitter := &notificationEmitter{
			logger:     logger,
			generation: generation,
			channel: notificationChannel{
				id:          channelCfg.ID,
				displayName: channelCfg.DisplayName,
				kind:        channelCfg.Kind,
				families:    families,
				types:       types,
				delivery:    channelCfg.Delivery,
				webhook:     channelCfg.Webhook,
				slack:       channelCfg.Slack,
			},
			normalQueue:    make(chan model.RuntimeEvent, channelCfg.Delivery.QueueSize),
			criticalQueue:  make(chan model.RuntimeEvent, channelCfg.Delivery.CriticalQueueSize),
			deliveryResult: deliveryResult,
		}
		emitter.wg.Add(2)
		go emitter.worker(emitter.criticalQueue)
		go emitter.worker(emitter.normalQueue)
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
		if !emitter.subscribed(event) {
			continue
		}
		err, shouldReport := emitter.emit(event)
		if shouldReport && n.enqueueResult != nil {
			go n.enqueueResult(emitter.generation, emitter.channel.id, err)
		}
		if err != nil {
			n.logger.Warn("notification queue is full", "channel_id", emitter.channel.id, "event_type", event.Type, "event_id", event.EventID)
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

func (e *notificationEmitter) subscribed(event model.RuntimeEvent) bool {
	if e == nil {
		return false
	}
	if len(e.channel.types) > 0 {
		if _, ok := e.channel.types[event.Type]; ok {
			return true
		}
	}
	if len(e.channel.families) > 0 {
		_, ok := e.channel.families[event.Family]
		return ok
	}
	return false
}

func (e *notificationEmitter) emit(event model.RuntimeEvent) (error, bool) {
	if e == nil {
		return nil, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return fmt.Errorf("notification notifier is closed"), true
	}

	queue := e.normalQueue
	if event.Priority == model.RuntimeEventPriorityCritical {
		queue = e.criticalQueue
	}

	select {
	case queue <- event:
		if !e.overflowActive {
			return nil, false
		}
		e.overflowActive = false
		return nil, true
	default:
		e.overflowActive = true
		return fmt.Errorf("notification queue is full"), true
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
		close(e.normalQueue)
		close(e.criticalQueue)
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

func (e *notificationEmitter) worker(queue <-chan model.RuntimeEvent) {
	defer e.wg.Done()
	for event := range queue {
		err := e.deliver(event)
		if e.deliveryResult != nil {
			e.deliveryResult(e.generation, e.channel.id, err)
		}
	}
}

func (e *notificationEmitter) deliver(event model.RuntimeEvent) error {
	payload, contentType, err := encodeNotificationEvent(e.channel.kind, event)
	if err != nil {
		return err
	}

	var (
		lastErr error
		urlText string
		headers map[string]string
	)
	switch e.channel.kind {
	case model.NotificationChannelKindWebhook:
		if e.channel.webhook != nil {
			urlText = e.channel.webhook.URL
			headers = e.channel.webhook.Headers
		}
	case model.NotificationChannelKindSlack:
		if e.channel.slack != nil {
			urlText = e.channel.slack.IncomingWebhookURL
		}
	default:
		return fmt.Errorf("unsupported notification channel kind %q", e.channel.kind)
	}

	client := &http.Client{Timeout: time.Duration(e.channel.delivery.TimeoutMS) * time.Millisecond}
	for attempt := 0; attempt <= e.channel.delivery.RetryCount; attempt++ {
		req, err := http.NewRequest(http.MethodPost, urlText, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", contentType)
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		resp, err := client.Do(req)
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

		if attempt >= e.channel.delivery.RetryCount {
			break
		}
		time.Sleep(time.Duration(e.channel.delivery.RetryDelayMS) * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("notification delivery failed")
	}
	return lastErr
}

func encodeNotificationEvent(kind model.NotificationChannelKind, event model.RuntimeEvent) ([]byte, string, error) {
	switch kind {
	case model.NotificationChannelKindSlack:
		raw, err := json.Marshal(map[string]string{"text": renderSlackText(event)})
		return raw, "application/json", err
	case model.NotificationChannelKindWebhook:
		raw, err := json.Marshal(event)
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
	o.notifierGeneration++
	next := newAsyncNotifier(o.currentConfig().Notifications, o.logger, o.notifierGeneration, o.reportNotificationDeliveryResult, o.reportNotificationEnqueueResult)
	previous := o.notifier
	o.notifier = next
	return previous
}

func (o *Orchestrator) reconcileNotificationHealthLocked() {
	active := make(map[string]model.NotificationChannelConfig, len(o.currentConfig().Notifications.Channels))
	for _, channel := range o.currentConfig().Notifications.Channels {
		active[channel.ID] = channel
		entry := o.notificationHealth[channel.ID]
		if entry == nil {
			entry = &NotificationChannelHealthSnapshot{
				ChannelID:   channel.ID,
				DisplayName: channel.DisplayName,
				Status:      "healthy",
			}
			o.notificationHealth[channel.ID] = entry
			continue
		}
		entry.DisplayName = channel.DisplayName
	}
	for channelID := range o.notificationHealth {
		if _, ok := active[channelID]; !ok {
			delete(o.notificationHealth, channelID)
			o.clearHealthAlertLocked(notificationQueueOverflowCode(channelID))
			o.clearHealthAlertLocked(notificationDeliveryFailedCode(channelID))
		}
	}
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
	if key := notificationFingerprint(event); key != "" {
		if _, ok := o.emittedNotificationKeys[key]; ok {
			return
		}
		o.emittedNotificationKeys[key] = struct{}{}
	}
	o.notifier.Emit(event)
}

func notificationFingerprint(event model.RuntimeEvent) string {
	issueID := ""
	if event.Subject != nil {
		issueID = strings.TrimSpace(event.Subject.IssueID)
	}
	switch event.Type {
	case model.NotificationEventIssueCompleted, model.NotificationEventIssueFailed:
		if issueID == "" {
			return ""
		}
		return fmt.Sprintf("%s|%s", event.Type, issueID)
	case model.NotificationEventIssueInterventionRequired:
		if issueID == "" {
			return ""
		}
		attemptCount := 0
		reason := ""
		if event.Dispatch != nil {
			attemptCount = event.Dispatch.AttemptCount
			if event.Dispatch.ContinuationReason != nil {
				reason = strings.TrimSpace(*event.Dispatch.ContinuationReason)
			}
		}
		return fmt.Sprintf("%s|%s|%d|%s", event.Type, issueID, attemptCount, reason)
	default:
		return ""
	}
}

func notificationQueueOverflowCode(channelID string) string {
	return notificationQueueOverflowPrefix + model.SanitizeWorkspaceKey(channelID)
}

func notificationDeliveryFailedCode(channelID string) string {
	return notificationDeliveryFailPrefix + model.SanitizeWorkspaceKey(channelID)
}

func (o *Orchestrator) reportNotificationEnqueueResult(generation uint64, channelID string, err error) {
	o.handleNotificationEnqueueResult(generation, channelID, err)
}

func (o *Orchestrator) reportNotificationDeliveryResult(generation uint64, channelID string, err error) {
	o.handleNotificationDeliveryResult(generation, channelID, err)
}

func (o *Orchestrator) ensureNotificationChannelHealthLocked(channelID string) *NotificationChannelHealthSnapshot {
	if entry := o.notificationHealth[channelID]; entry != nil {
		return entry
	}
	entry := &NotificationChannelHealthSnapshot{
		ChannelID: channelID,
		Status:    "healthy",
	}
	for _, channel := range o.currentConfig().Notifications.Channels {
		if channel.ID == channelID {
			entry.DisplayName = channel.DisplayName
			break
		}
	}
	o.notificationHealth[channelID] = entry
	return entry
}

func (o *Orchestrator) handleNotificationEnqueueResult(generation uint64, channelID string, err error) {
	code := notificationQueueOverflowCode(channelID)

	o.mu.Lock()
	defer o.mu.Unlock()
	if generation != o.notifierGeneration {
		o.logger.Debug("ignoring stale notification enqueue result", "channel_id", channelID, "generation", generation, "active_generation", o.notifierGeneration)
		return
	}

	entry := o.ensureNotificationChannelHealthLocked(channelID)
	now := o.now().UTC()
	entry.LastAttemptAt = cloneTimePtr(&now)

	if err == nil {
		entry.QueueOverflow = false
		if entry.ConsecutiveFailures == 0 {
			entry.Status = "healthy"
			entry.LastError = nil
		}
		o.clearHealthAlertAndNotifyLocked(code)
		o.publishViewLocked()
		return
	}

	errorText := err.Error()
	entry.QueueOverflow = true
	entry.Status = "degraded"
	entry.LastError = optionalError(errorText)
	o.setHealthAlertAndNotifyLocked(AlertSnapshot{
		Code:    code,
		Level:   "warn",
		Message: errorText,
	})
	o.publishViewLocked()
}

func (o *Orchestrator) handleNotificationDeliveryResult(generation uint64, channelID string, err error) {
	code := notificationDeliveryFailedCode(channelID)

	o.mu.Lock()
	defer o.mu.Unlock()
	if generation != o.notifierGeneration {
		o.logger.Debug("ignoring stale notification delivery result", "channel_id", channelID, "generation", generation, "active_generation", o.notifierGeneration)
		return
	}

	entry := o.ensureNotificationChannelHealthLocked(channelID)
	now := o.now().UTC()
	entry.LastAttemptAt = cloneTimePtr(&now)

	if err == nil {
		entry.Status = "healthy"
		entry.LastSuccessAt = cloneTimePtr(&now)
		entry.ConsecutiveFailures = 0
		if !entry.QueueOverflow {
			entry.LastError = nil
		}
		o.clearHealthAlertAndNotifyLocked(code)
		o.publishViewLocked()
		return
	}

	errorText := err.Error()
	entry.Status = "degraded"
	entry.LastError = optionalError(errorText)
	entry.ConsecutiveFailures++
	o.logger.Warn("notification delivery failed", "channel_id", channelID, "error", errorText)
	o.setHealthAlertAndNotifyLocked(AlertSnapshot{
		Code:    code,
		Level:   "warn",
		Message: fmt.Sprintf("notification channel %s failed: %s", channelID, errorText),
	})
	o.publishViewLocked()
}

func renderSlackText(event model.RuntimeEvent) string {
	lines := []string{
		fmt.Sprintf("[%s] %s", strings.ToUpper(strings.TrimSpace(event.Level)), string(event.Type)),
	}
	if strings.TrimSpace(event.Summary) != "" {
		lines = append(lines, event.Summary)
	}
	if event.Subject != nil {
		if strings.TrimSpace(event.Subject.Identifier) != "" {
			lines = append(lines, fmt.Sprintf("Issue: %s", event.Subject.Identifier))
		} else if strings.TrimSpace(event.Subject.IssueID) != "" {
			lines = append(lines, fmt.Sprintf("IssueID: %s", event.Subject.IssueID))
		}
		if strings.TrimSpace(event.Subject.WorkspacePath) != "" {
			lines = append(lines, fmt.Sprintf("Workspace: %s", event.Subject.WorkspacePath))
		}
	}
	if event.Dispatch != nil {
		if strings.TrimSpace(event.Dispatch.Kind) != "" {
			lines = append(lines, fmt.Sprintf("Dispatch: %s", event.Dispatch.Kind))
		}
		if strings.TrimSpace(event.Dispatch.ExpectedOutcome) != "" {
			lines = append(lines, fmt.Sprintf("Expected: %s", event.Dispatch.ExpectedOutcome))
		}
		if event.Dispatch.ContinuationReason != nil && strings.TrimSpace(*event.Dispatch.ContinuationReason) != "" {
			lines = append(lines, fmt.Sprintf("Reason: %s", *event.Dispatch.ContinuationReason))
		}
	}
	if event.Failure != nil {
		if strings.TrimSpace(event.Failure.Phase) != "" {
			lines = append(lines, fmt.Sprintf("Phase: %s", event.Failure.Phase))
		}
		if strings.TrimSpace(event.Failure.Error) != "" {
			lines = append(lines, fmt.Sprintf("Error: %s", event.Failure.Error))
		}
	}
	if event.PullRequest != nil {
		if event.PullRequest.Number > 0 {
			lines = append(lines, fmt.Sprintf("PR: #%d", event.PullRequest.Number))
		}
		if strings.TrimSpace(event.PullRequest.State) != "" {
			lines = append(lines, fmt.Sprintf("PR State: %s", event.PullRequest.State))
		}
		if strings.TrimSpace(event.PullRequest.URL) != "" {
			lines = append(lines, fmt.Sprintf("PR URL: %s", event.PullRequest.URL))
		}
	}
	if event.Alert != nil {
		if strings.TrimSpace(event.Alert.Code) != "" {
			lines = append(lines, fmt.Sprintf("Alert: %s", event.Alert.Code))
		}
		if strings.TrimSpace(event.Alert.Status) != "" {
			lines = append(lines, fmt.Sprintf("Alert Status: %s", event.Alert.Status))
		}
	}
	return strings.Join(lines, "\n")
}

func eventSubject(issueID string, identifier string, workspacePath string) *model.RuntimeEventSubject {
	if strings.TrimSpace(issueID) == "" && strings.TrimSpace(identifier) == "" && strings.TrimSpace(workspacePath) == "" {
		return nil
	}
	return &model.RuntimeEventSubject{
		IssueID:       issueID,
		Identifier:    identifier,
		WorkspacePath: workspacePath,
	}
}

func eventDispatch(dispatch *model.DispatchContext, attempt int) *model.RuntimeEventDispatch {
	if dispatch == nil && attempt <= 0 {
		return nil
	}
	result := &model.RuntimeEventDispatch{
		AttemptCount: attemptCountFromRetry(attempt),
	}
	if dispatch != nil {
		result.Kind = string(dispatch.Kind)
		result.ExpectedOutcome = string(dispatch.ExpectedOutcome)
		result.ContinuationReason = cloneContinuationReason(dispatch)
	}
	return result
}

func eventPullRequest(pr *PullRequestInfo, branch string) *model.RuntimeEventPullRequest {
	if pr == nil && strings.TrimSpace(branch) == "" {
		return nil
	}
	result := &model.RuntimeEventPullRequest{Branch: branch}
	if pr != nil {
		result.Number = pr.Number
		result.URL = pr.URL
		result.State = string(pr.State)
		if strings.TrimSpace(result.Branch) == "" {
			result.Branch = pr.HeadBranch
		}
	}
	return result
}

func (o *Orchestrator) newIssueDispatchedEvent(issue model.Issue, attempt int, dispatch *model.DispatchContext) model.RuntimeEvent {
	return model.RuntimeEvent{
		Version:    runtimeEventVersion,
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueDispatched,
		Family:     model.RuntimeEventFamilyIssue,
		Priority:   model.RuntimeEventPriorityNormal,
		Level:      "info",
		OccurredAt: o.now().UTC(),
		Summary:    fmt.Sprintf("issue %s dispatched", issue.Identifier),
		Subject:    eventSubject(issue.ID, issue.Identifier, ""),
		Dispatch:   eventDispatch(dispatch, attempt),
	}
}

func (o *Orchestrator) newIssueFailedEvent(issueID string, identifier string, workspacePath string, phase model.RunPhase, attempt int, err error, dispatch *model.DispatchContext) model.RuntimeEvent {
	return model.RuntimeEvent{
		Version:    runtimeEventVersion,
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueFailed,
		Family:     model.RuntimeEventFamilyIssue,
		Priority:   model.RuntimeEventPriorityNormal,
		Level:      "warn",
		OccurredAt: o.now().UTC(),
		Summary:    fmt.Sprintf("issue %s failed", identifier),
		Subject:    eventSubject(issueID, identifier, workspacePath),
		Dispatch:   eventDispatch(dispatch, attempt),
		Failure: &model.RuntimeEventFailure{
			Phase: phase.String(),
			Error: errorString(err),
		},
	}
}

func (o *Orchestrator) newIssueCompletedEvent(issueID string, identifier string) model.RuntimeEvent {
	return model.RuntimeEvent{
		Version:    runtimeEventVersion,
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueCompleted,
		Family:     model.RuntimeEventFamilyIssue,
		Priority:   model.RuntimeEventPriorityNormal,
		Level:      "info",
		OccurredAt: o.now().UTC(),
		Summary:    fmt.Sprintf("issue %s completed", identifier),
		Subject:    eventSubject(issueID, identifier, ""),
	}
}

func (o *Orchestrator) newIssueInterventionRequiredEvent(issueID string, identifier string, branch string, reason string, expectedOutcome model.CompletionMode, attempt int, pr *PullRequestInfo) model.RuntimeEvent {
	return model.RuntimeEvent{
		Version:    runtimeEventVersion,
		EventID:    o.nextEventID(),
		Type:       model.NotificationEventIssueInterventionRequired,
		Family:     model.RuntimeEventFamilyIssue,
		Priority:   model.RuntimeEventPriorityCritical,
		Level:      "warn",
		OccurredAt: o.now().UTC(),
		Summary:    fmt.Sprintf("issue %s requires manual intervention", identifier),
		Subject:    eventSubject(issueID, identifier, ""),
		Dispatch: &model.RuntimeEventDispatch{
			AttemptCount:       attemptCountFromRetry(attempt),
			ExpectedOutcome:    string(expectedOutcome),
			ContinuationReason: optionalError(reason),
		},
		PullRequest: eventPullRequest(pr, branch),
	}
}

func (o *Orchestrator) newSystemAlertEvent(eventType model.NotificationEventType, alert AlertSnapshot) model.RuntimeEvent {
	status := "active"
	if eventType == model.NotificationEventSystemAlertCleared {
		status = "cleared"
	}
	return model.RuntimeEvent{
		Version:    runtimeEventVersion,
		EventID:    o.nextEventID(),
		Type:       eventType,
		Family:     model.RuntimeEventFamilyHealth,
		Priority:   model.RuntimeEventPriorityCritical,
		Level:      alert.Level,
		OccurredAt: o.now().UTC(),
		Summary:    alert.Message,
		Subject:    eventSubject(alert.IssueID, alert.IssueIdentifier, ""),
		Alert: &model.RuntimeEventAlert{
			Code:    alert.Code,
			Status:  status,
			Level:   alert.Level,
			Message: alert.Message,
		},
	}
}

func cloneContinuationReason(dispatch *model.DispatchContext) *string {
	if dispatch == nil || dispatch.Reason == nil {
		return nil
	}
	reason := string(*dispatch.Reason)
	return &reason
}
