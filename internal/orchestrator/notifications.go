package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"symphony-go/internal/model"
)

type asyncNotifier struct {
	logger         *slog.Logger
	channels       []notificationChannel
	defaults       model.NotificationDefaultsConfig
	deliveryResult func(channel string, err error)
}

type notificationChannel struct {
	name    string
	kind    model.NotificationChannelKind
	url     string
	headers map[string]string
	events  map[model.NotificationEventType]struct{}
}

type webhookPayload struct {
	Type       string         `json:"type"`
	Level      string         `json:"level"`
	Timestamp  string         `json:"timestamp"`
	IssueID    string         `json:"issue_id,omitempty"`
	Identifier string         `json:"identifier,omitempty"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}

type slackPayload struct {
	Text string `json:"text"`
}

func newAsyncNotifier(cfg model.NotificationsConfig, logger *slog.Logger, deliveryResult func(channel string, err error)) *asyncNotifier {
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
	return &asyncNotifier{
		logger:         logger,
		channels:       channels,
		defaults:       cfg.Defaults,
		deliveryResult: deliveryResult,
	}
}

func (n *asyncNotifier) Emit(event model.NotificationEvent) {
	if n == nil {
		return
	}
	for _, channel := range n.channels {
		channel := channel
		if len(channel.events) > 0 {
			if _, ok := channel.events[event.Type]; !ok {
				continue
			}
		}
		go func() {
			err := n.deliver(channel, event)
			if n.deliveryResult != nil {
				n.deliveryResult(channel.name, err)
			}
		}()
	}
}

func (n *asyncNotifier) Close() {}

func (n *asyncNotifier) deliver(channel notificationChannel, event model.NotificationEvent) error {
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

		client := &http.Client{Timeout: time.Duration(n.defaults.TimeoutMS) * time.Millisecond}
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

func (n *asyncNotifier) encode(kind model.NotificationChannelKind, event model.NotificationEvent) ([]byte, string, error) {
	switch kind {
	case model.NotificationChannelKindSlack:
		payload := slackPayload{Text: renderSlackText(event)}
		raw, err := json.Marshal(payload)
		return raw, "application/json", err
	case model.NotificationChannelKindWebhook:
		raw, err := json.Marshal(webhookPayload{
			Type:       string(event.Type),
			Level:      event.Level,
			Timestamp:  event.Timestamp.UTC().Format(time.RFC3339),
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

func (o *Orchestrator) emitNotificationLocked(event model.NotificationEvent) {
	if o.notifier == nil {
		return
	}
	o.notifier.Emit(event)
}

func (o *Orchestrator) handleNotificationDeliveryResult(channel string, err error) {
	code := "notification_delivery_failed_" + model.SanitizeWorkspaceKey(channel)
	if err == nil {
		o.mu.Lock()
		if _, exists := o.systemAlerts[code]; !exists {
			o.mu.Unlock()
			return
		}
		delete(o.systemAlerts, code)
		o.refreshSnapshotLocked()
		o.publishSnapshotLocked()
		o.mu.Unlock()
		return
	}

	o.logger.Warn("notification delivery failed", "channel", channel, "error", err.Error())
	o.mu.Lock()
	o.systemAlerts[code] = AlertSnapshot{
		Code:    code,
		Level:   "warn",
		Message: fmt.Sprintf("notification channel %s failed: %s", channel, err.Error()),
	}
	o.refreshSnapshotLocked()
	o.publishSnapshotLocked()
	o.mu.Unlock()
}

func shouldSuppressAlertNotification(code string) bool {
	return strings.HasPrefix(code, "notification_delivery_failed_")
}

func renderSlackText(event model.NotificationEvent) string {
	lines := []string{
		fmt.Sprintf("[%s] %s", strings.ToUpper(strings.TrimSpace(event.Level)), string(event.Type)),
		event.Message,
	}
	if strings.TrimSpace(event.Identifier) != "" {
		lines = append(lines, fmt.Sprintf("Issue: %s", event.Identifier))
	}
	if strings.TrimSpace(event.IssueID) != "" && strings.TrimSpace(event.Identifier) == "" {
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
		lines = append(lines, fmt.Sprintf("%s: %v", key, event.Details[key]))
	}
	return strings.Join(lines, "\n")
}

func cloneDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(details))
	for key, value := range details {
		cloned[key] = value
	}
	return cloned
}

func (o *Orchestrator) newIssueDispatchedEvent(issue model.Issue, attempt int, dispatch *model.DispatchContext) model.NotificationEvent {
	details := map[string]any{
		"state":         issue.State,
		"attempt_count": attemptCountFromRetry(attempt),
	}
	if dispatch != nil {
		details["dispatch_kind"] = string(dispatch.Kind)
		details["expected_outcome"] = string(dispatch.ExpectedOutcome)
		if dispatch.Reason != nil {
			details["continuation_reason"] = string(*dispatch.Reason)
		}
	}
	return model.NotificationEvent{
		Type:       model.NotificationEventIssueDispatched,
		Level:      "info",
		Timestamp:  o.now().UTC(),
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Message:    fmt.Sprintf("issue %s dispatched", issue.Identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newIssueFailedEvent(issueID string, identifier string, workspacePath string, phase model.RunPhase, attempt int, err error, dispatch *model.DispatchContext) model.NotificationEvent {
	details := map[string]any{
		"run_phase":      phase.String(),
		"attempt_count":  attemptCountFromRetry(attempt),
		"workspace_path": workspacePath,
		"error":          errorString(err),
	}
	if dispatch != nil {
		details["dispatch_kind"] = string(dispatch.Kind)
		details["expected_outcome"] = string(dispatch.ExpectedOutcome)
	}
	return model.NotificationEvent{
		Type:       model.NotificationEventIssueFailed,
		Level:      "warn",
		Timestamp:  o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s failed", identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newIssueCompletedEvent(issueID string, identifier string) model.NotificationEvent {
	return model.NotificationEvent{
		Type:       model.NotificationEventIssueCompleted,
		Level:      "info",
		Timestamp:  o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s completed", identifier),
	}
}

func (o *Orchestrator) newIssueInterventionRequiredEvent(issueID string, identifier string, branch string, reason string, expectedOutcome model.CompletionMode, pr *PullRequestInfo) model.NotificationEvent {
	details := map[string]any{
		"branch":           branch,
		"reason":           reason,
		"expected_outcome": string(expectedOutcome),
	}
	if pr != nil {
		details["pr_number"] = pr.Number
		details["pr_url"] = pr.URL
		details["pr_state"] = string(pr.State)
	}
	return model.NotificationEvent{
		Type:       model.NotificationEventIssueInterventionRequired,
		Level:      "warn",
		Timestamp:  o.now().UTC(),
		IssueID:    issueID,
		Identifier: identifier,
		Message:    fmt.Sprintf("issue %s requires manual intervention", identifier),
		Details:    details,
	}
}

func (o *Orchestrator) newSystemAlertEvent(eventType model.NotificationEventType, alert AlertSnapshot) model.NotificationEvent {
	details := map[string]any{
		"code": alert.Code,
	}
	if strings.TrimSpace(alert.Level) != "" {
		details["alert_level"] = alert.Level
	}
	return model.NotificationEvent{
		Type:       eventType,
		Level:      alert.Level,
		Timestamp:  o.now().UTC(),
		IssueID:    alert.IssueID,
		Identifier: alert.IssueIdentifier,
		Message:    alert.Message,
		Details:    details,
	}
}
