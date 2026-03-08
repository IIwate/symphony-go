package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"symphony-go/internal/model"
)

const (
	githubAPIVersion          = "2022-11-28"
	githubPageSize            = 100
	githubRefreshConcurrency  = 10
	githubLowRemainingWarning = 10
)

type GitHubClient struct {
	httpClient     *http.Client
	configProvider func() *model.ServiceConfig
}

type githubIssue struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	State       string           `json:"state"`
	HTMLURL     string           `json:"html_url"`
	Labels      []githubLabel    `json:"labels"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
	PullRequest *json.RawMessage `json:"pull_request"`
}

type githubLabel struct {
	Name string `json:"name"`
}

type githubStateQuery struct {
	issueState string
	label      string
}

type githubIssueOptions struct {
	errorOnPullRequest bool
	includeStateless   bool
}

func NewGitHubClient(cfg *model.ServiceConfig, httpClient *http.Client) (*GitHubClient, error) {
	return NewDynamicGitHubClient(func() *model.ServiceConfig { return cfg }, httpClient)
}

func NewDynamicGitHubClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (*GitHubClient, error) {
	if configProvider == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config provider is nil", nil)
	}
	cfg := configProvider()
	if cfg == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config is nil", nil)
	}
	if strings.TrimSpace(cfg.TrackerAPIKey) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerAPIKey, "tracker.api_key is required", nil)
	}
	if strings.TrimSpace(cfg.TrackerOwner) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerOwner, "tracker.owner is required", nil)
	}
	if strings.TrimSpace(cfg.TrackerRepo) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerRepo, "tracker.repo is required", nil)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &GitHubClient{
		httpClient:     httpClient,
		configProvider: configProvider,
	}, nil
}

func (c *GitHubClient) FetchCandidateIssues(ctx context.Context) ([]model.Issue, error) {
	cfg := c.currentConfig()
	return c.fetchIssuesForStates(ctx, cfg.ActiveStates)
}

func (c *GitHubClient) FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error) {
	if len(states) == 0 {
		return []model.Issue{}, nil
	}

	return c.fetchIssuesForStates(ctx, states)
}

func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	if len(ids) == 0 {
		return []model.Issue{}, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]model.Issue, len(ids))
	sem := make(chan struct{}, githubRefreshConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	setErr := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if firstErr != nil {
			return
		}
		firstErr = err
		cancel()
	}

launchLoop:
	for index, rawID := range ids {
		issueID := strings.TrimSpace(rawID)
		if issueID == "" {
			setErr(model.NewTrackerError(model.ErrGitHubAPIRequest, "issue id is required", nil))
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			setErr(model.NewTrackerError(model.ErrGitHubAPIRequest, "wait for GitHub refresh slot", ctx.Err()))
			break launchLoop
		}

		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()

			issue, err := c.fetchIssueByID(ctx, id)
			if err != nil {
				setErr(err)
				return
			}
			results[i] = issue
		}(index, issueID)
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	return results, nil
}

func (c *GitHubClient) fetchIssuesForStates(ctx context.Context, states []string) ([]model.Issue, error) {
	queries := c.buildStateQueries(states)
	if len(queries) == 0 {
		return []model.Issue{}, nil
	}

	issues := make([]model.Issue, 0)
	seen := make(map[string]struct{})

	for _, query := range queries {
		nextURL := ""
		for {
			pageIssues, next, err := c.fetchIssuePage(ctx, nextURL, query)
			if err != nil {
				return nil, err
			}
			for _, node := range pageIssues {
				issue, include, err := c.normalizeGitHubIssue(ctx, node, githubIssueOptions{})
				if err != nil {
					return nil, err
				}
				if !include {
					continue
				}
				if _, ok := seen[issue.ID]; ok {
					continue
				}
				seen[issue.ID] = struct{}{}
				issues = append(issues, issue)
			}
			if next == "" {
				break
			}
			nextURL = next
		}
	}

	return issues, nil
}

func (c *GitHubClient) fetchIssuePage(ctx context.Context, nextURL string, query githubStateQuery) ([]githubIssue, string, error) {
	requestURL := nextURL
	var values url.Values
	if requestURL == "" {
		values = url.Values{}
		values.Set("state", query.issueState)
		if query.label != "" {
			values.Set("labels", query.label)
		}
		values.Set("per_page", strconv.Itoa(githubPageSize))
		requestURL = c.issuesPath()
	}

	var issues []githubIssue
	headers, err := c.doGitHubJSON(ctx, http.MethodGet, requestURL, values, &issues)
	if err != nil {
		return nil, "", err
	}

	return issues, parseGitHubNextLink(headers.Get("Link")), nil
}

func (c *GitHubClient) fetchIssueByID(ctx context.Context, id string) (model.Issue, error) {
	var issue githubIssue
	_, err := c.doGitHubJSON(ctx, http.MethodGet, c.issuePath(id), nil, &issue)
	if err != nil {
		return model.Issue{}, err
	}

	normalized, include, err := c.normalizeGitHubIssue(ctx, issue, githubIssueOptions{
		errorOnPullRequest: true,
		includeStateless:   true,
	})
	if err != nil {
		return model.Issue{}, err
	}
	if !include {
		return model.Issue{}, model.NewTrackerError(model.ErrGitHubUnknownPayload, fmt.Sprintf("GitHub issue %q was not returned", id), nil)
	}

	return normalized, nil
}

func (c *GitHubClient) doGitHubJSON(ctx context.Context, method string, requestPath string, query url.Values, target any) (http.Header, error) {
	requestURL, err := c.buildGitHubURL(requestPath, query)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "build GitHub request URL", err)
	}

	cfg := c.currentConfig()
	req, err := http.NewRequestWithContext(ctx, method, requestURL, nil)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "build GitHub request", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.TrackerAPIKey)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "execute GitHub request", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "read GitHub response", err)
	}

	if err := c.checkGitHubResponse(ctx, resp.StatusCode, resp.Header, rawBody); err != nil {
		return nil, err
	}
	if target == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(rawBody, target); err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubUnknownPayload, "decode GitHub payload", err)
	}

	return resp.Header, nil
}

func (c *GitHubClient) checkGitHubResponse(ctx context.Context, statusCode int, headers http.Header, rawBody []byte) error {
	message := extractGitHubMessage(rawBody)
	if statusCode == http.StatusTooManyRequests || isGitHubSecondaryRateLimit(statusCode, message) {
		return model.NewTrackerError(model.ErrGitHubRateLimit, buildGitHubRateLimitMessage("GitHub secondary rate limit hit", headers), nil)
	}

	if remaining, ok := parseGitHubRemaining(headers); ok {
		if remaining == 0 {
			return model.NewTrackerError(model.ErrGitHubRateLimit, buildGitHubRateLimitMessage("GitHub primary rate limit exhausted", headers), nil)
		}
		if remaining <= githubLowRemainingWarning {
			cfg := c.currentConfig()
			slog.WarnContext(ctx, "github tracker rate limit remaining is low", "remaining", remaining, "poll_interval_ms", cfg.PollIntervalMS)
		}
	}

	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return model.NewTrackerError(model.ErrGitHubPermissionDenied, fmt.Sprintf("GitHub access denied with status %d%s", statusCode, formatGitHubMessage(message)), nil)
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return model.NewTrackerError(model.ErrGitHubAPIStatus, fmt.Sprintf("unexpected GitHub status %d%s", statusCode, formatGitHubMessage(message)), nil)
	}

	return nil
}

func (c *GitHubClient) normalizeGitHubIssue(ctx context.Context, node githubIssue, options githubIssueOptions) (model.Issue, bool, error) {
	cfg := c.currentConfig()
	if node.Number <= 0 {
		return model.Issue{}, false, model.NewTrackerError(model.ErrGitHubUnknownPayload, "GitHub issue number is missing", nil)
	}
	if node.PullRequest != nil {
		if options.errorOnPullRequest {
			return model.Issue{}, false, model.NewTrackerError(model.ErrGitHubUnexpectedPullRequest, fmt.Sprintf("GitHub issue %s/%s#%d resolved to a pull request", cfg.TrackerOwner, cfg.TrackerRepo, node.Number), nil)
		}
		return model.Issue{}, false, nil
	}

	labels := normalizeGitHubLabels(node.Labels)
	state, conflict := c.extractGitHubState(labels, node.State)
	if conflict {
		slog.WarnContext(ctx, "github issue has multiple state labels", "issue_number", node.Number, "owner", cfg.TrackerOwner, "repo", cfg.TrackerRepo)
		if !options.includeStateless {
			return model.Issue{}, false, nil
		}
		state = ""
	}
	if state == "" && !options.includeStateless {
		return model.Issue{}, false, nil
	}

	branchName := fmt.Sprintf("issue-%d", node.Number)
	issue := model.Issue{
		ID:         strconv.Itoa(node.Number),
		Identifier: fmt.Sprintf("%s/%s#%d", cfg.TrackerOwner, cfg.TrackerRepo, node.Number),
		Title:      strings.TrimSpace(node.Title),
		State:      state,
		BranchName: &branchName,
		Labels:     labels,
		CreatedAt:  parseTime(node.CreatedAt),
		UpdatedAt:  parseTime(node.UpdatedAt),
	}
	if text := strings.TrimSpace(node.Body); text != "" {
		issue.Description = &text
	}
	if text := strings.TrimSpace(node.HTMLURL); text != "" {
		issue.URL = &text
	}

	return issue, true, nil
}

func (c *GitHubClient) extractGitHubState(labels []string, issueState string) (string, bool) {
	cfg := c.currentConfig()
	prefix := model.NormalizeState(cfg.TrackerStateLabelPrefix)
	if prefix == "" {
		prefix = "symphony:"
	}

	stateLabels := make(map[string]struct{})
	for _, label := range labels {
		if !strings.HasPrefix(label, prefix) {
			continue
		}
		state := strings.TrimSpace(strings.TrimPrefix(label, prefix))
		if state == "" {
			continue
		}
		stateLabels[state] = struct{}{}
	}

	normalizedIssueState := model.NormalizeState(issueState)
	if normalizedIssueState == "closed" {
		for _, terminalState := range cfg.TerminalStates {
			normalizedTerminal := model.NormalizeState(terminalState)
			if normalizedTerminal == "" || normalizedTerminal == "closed" {
				continue
			}
			if _, ok := stateLabels[normalizedTerminal]; ok {
				return normalizedTerminal, false
			}
		}
		return "closed", false
	}

	if len(stateLabels) == 0 {
		return "", false
	}
	if len(stateLabels) > 1 {
		return "", true
	}
	for state := range stateLabels {
		return state, false
	}

	return "", false
}

func (c *GitHubClient) buildStateQueries(states []string) []githubStateQuery {
	cfg := c.currentConfig()
	prefix := strings.TrimSpace(cfg.TrackerStateLabelPrefix)
	if prefix == "" {
		prefix = "symphony:"
	}

	terminalStates := make(map[string]struct{}, len(cfg.TerminalStates))
	for _, state := range cfg.TerminalStates {
		normalized := model.NormalizeState(state)
		if normalized == "" {
			continue
		}
		terminalStates[normalized] = struct{}{}
	}

	queries := make([]githubStateQuery, 0, len(states))
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		normalized := model.NormalizeState(state)
		if normalized == "" {
			continue
		}

		query := githubStateQuery{
			issueState: "open",
			label:      prefix + normalized,
		}
		if normalized == "closed" {
			query.issueState = "closed"
			query.label = ""
		} else if _, ok := terminalStates[normalized]; ok {
			query.issueState = "closed"
		}

		key := query.issueState + "|" + query.label
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		queries = append(queries, query)
	}

	return queries
}

func (c *GitHubClient) buildGitHubURL(requestPath string, query url.Values) (string, error) {
	if strings.HasPrefix(requestPath, "http://") || strings.HasPrefix(requestPath, "https://") {
		parsed, err := url.Parse(requestPath)
		if err != nil {
			return "", err
		}
		return parsed.String(), nil
	}

	cfg := c.currentConfig()
	base, err := url.Parse(strings.TrimSpace(cfg.TrackerEndpoint))
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(requestPath, "/")
	if query != nil {
		base.RawQuery = query.Encode()
	} else {
		base.RawQuery = ""
	}

	return base.String(), nil
}

func (c *GitHubClient) issuesPath() string {
	cfg := c.currentConfig()
	return fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(cfg.TrackerOwner), url.PathEscape(cfg.TrackerRepo))
}

func (c *GitHubClient) issuePath(id string) string {
	cfg := c.currentConfig()
	return fmt.Sprintf("/repos/%s/%s/issues/%s", url.PathEscape(cfg.TrackerOwner), url.PathEscape(cfg.TrackerRepo), url.PathEscape(strings.TrimSpace(id)))
}

func (c *GitHubClient) currentConfig() *model.ServiceConfig {
	cfg := &model.ServiceConfig{}
	if c.configProvider != nil && c.configProvider() != nil {
		clone := *c.configProvider()
		cfg = &clone
	}
	if strings.TrimSpace(cfg.TrackerEndpoint) == "" {
		cfg.TrackerEndpoint = "https://api.github.com"
	}
	if strings.TrimSpace(cfg.TrackerStateLabelPrefix) == "" {
		cfg.TrackerStateLabelPrefix = "symphony:"
	}
	if len(cfg.ActiveStates) == 0 {
		cfg.ActiveStates = []string{"todo", "in-progress"}
	} else {
		cfg.ActiveStates = append([]string(nil), cfg.ActiveStates...)
	}
	if len(cfg.TerminalStates) == 0 {
		cfg.TerminalStates = []string{"closed", "cancelled"}
	} else {
		cfg.TerminalStates = append([]string(nil), cfg.TerminalStates...)
	}

	return cfg
}

func normalizeGitHubLabels(labels []githubLabel) []string {
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		if value := model.NormalizeState(label.Name); value != "" {
			normalized = append(normalized, value)
		}
	}

	return normalized
}

func parseGitHubNextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		trimmed := strings.TrimSpace(part)
		if !strings.Contains(trimmed, `rel="next"`) {
			continue
		}
		start := strings.Index(trimmed, "<")
		end := strings.Index(trimmed, ">")
		if start >= 0 && end > start {
			return trimmed[start+1 : end]
		}
	}

	return ""
}

func parseGitHubRemaining(headers http.Header) (int, bool) {
	raw := strings.TrimSpace(headers.Get("X-RateLimit-Remaining"))
	if raw == "" {
		return 0, false
	}
	remaining, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}

	return remaining, true
}

func isGitHubSecondaryRateLimit(statusCode int, message string) bool {
	if statusCode != http.StatusForbidden {
		return false
	}
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "secondary rate limit")
}

func buildGitHubRateLimitMessage(prefix string, headers http.Header) string {
	hint := "retry on a later poll tick"
	if retryAfter := strings.TrimSpace(headers.Get("Retry-After")); retryAfter != "" {
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
			hint = fmt.Sprintf("retry after %ds", seconds)
		} else if parsed, err := http.ParseTime(retryAfter); err == nil {
			hint = fmt.Sprintf("retry after %s", parsed.UTC().Format(time.RFC3339))
		} else {
			hint = fmt.Sprintf("retry after %s", retryAfter)
		}
	} else if reset := strings.TrimSpace(headers.Get("X-RateLimit-Reset")); reset != "" {
		if unixSeconds, err := strconv.ParseInt(reset, 10, 64); err == nil {
			resetAt := time.Unix(unixSeconds, 0).UTC()
			hint = fmt.Sprintf("retry at %s", resetAt.Format(time.RFC3339))
		}
	}

	return prefix + "; " + hint
}

func extractGitHubMessage(rawBody []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rawBody, &payload); err == nil {
		if message := strings.TrimSpace(payload.Message); message != "" {
			return message
		}
	}

	message := strings.TrimSpace(string(rawBody))
	if len(message) > 160 {
		message = message[:160] + "..."
	}
	return message
}

func formatGitHubMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	return ": " + message
}
