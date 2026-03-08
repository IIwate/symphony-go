package tracker

import (
	"bytes"
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
	defaultGitHubEndpoint         = "https://api.github.com"
	defaultGitHubAPIVersion       = "2022-11-28"
	defaultGitHubPageSize         = 100
	defaultGitHubFetchConcurrency = 10
	defaultGitHubStateLabelPrefix = "symphony:"
)

var (
	defaultGitHubActiveStates   = []string{"todo", "in-progress"}
	defaultGitHubTerminalStates = []string{"closed", "cancelled"}
)

type GitHubClient struct {
	httpClient     *http.Client
	configProvider func() *model.ServiceConfig
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
	return c.fetchIssuesForStates(ctx, githubActiveStates(c.currentConfig()))
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

	results := make([]model.Issue, len(ids))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, defaultGitHubFetchConcurrency)

	for index, rawID := range ids {
		number, err := strconv.Atoi(strings.TrimSpace(rawID))
		if err != nil {
			return nil, model.NewTrackerError(model.ErrGitHubInvalidIssueID, fmt.Sprintf("invalid GitHub issue id %q", rawID), err)
		}

		wg.Add(1)
		go func(index int, number int) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() {
				<-sem
			}()

			issue, err := c.fetchIssueByNumber(ctx, number)
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}

			results[index] = issue
		}(index, number)
	}

	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	filtered := make([]model.Issue, 0, len(results))
	for _, issue := range results {
		if strings.TrimSpace(issue.ID) != "" {
			filtered = append(filtered, issue)
		}
	}

	return filtered, nil
}

func (c *GitHubClient) fetchIssuesForStates(ctx context.Context, states []string) ([]model.Issue, error) {
	cfg := c.currentConfig()
	issues := make([]model.Issue, 0)
	seen := make(map[string]struct{})

	for _, state := range states {
		normalizedState := model.NormalizeState(state)
		if normalizedState == "" {
			continue
		}

		nextURL := buildGitHubListURL(cfg, normalizedState)
		for nextURL != "" {
			var payload []gitHubIssue
			headers, err := c.getJSON(ctx, nextURL, &payload)
			if err != nil {
				return nil, err
			}

			for _, item := range payload {
				if item.PullRequest != nil {
					continue
				}

				issue, include := normalizeGitHubIssue(cfg, item)
				if !include {
					continue
				}
				if _, ok := seen[issue.ID]; ok {
					continue
				}

				seen[issue.ID] = struct{}{}
				issues = append(issues, issue)
			}

			nextURL = parseGitHubNextLink(headers.Get("Link"))
		}
	}

	return issues, nil
}

func (c *GitHubClient) fetchIssueByNumber(ctx context.Context, number int) (model.Issue, error) {
	cfg := c.currentConfig()
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d", githubEndpoint(cfg), url.PathEscape(strings.TrimSpace(cfg.TrackerOwner)), url.PathEscape(strings.TrimSpace(cfg.TrackerRepo)), number)

	var payload gitHubIssue
	if _, err := c.getJSON(ctx, endpoint, &payload); err != nil {
		return model.Issue{}, err
	}
	if payload.PullRequest != nil {
		return model.Issue{}, model.NewTrackerError(model.ErrGitHubUnexpectedPullRequest, fmt.Sprintf("GitHub issue %d resolved to a pull request", number), nil)
	}

	issue, _ := normalizeGitHubIssue(cfg, payload)
	return issue, nil
}

func (c *GitHubClient) getJSON(ctx context.Context, endpoint string, target any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "build GitHub request", err)
	}

	cfg := c.currentConfig()
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.TrackerAPIKey))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", defaultGitHubAPIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "request GitHub API", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "read GitHub response", err)
	}

	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) && isGitHubRateLimited(resp.Header, rawBody) {
		return nil, model.NewTrackerError(model.ErrGitHubRateLimited, buildGitHubRateLimitMessage(resp, rawBody), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, model.NewTrackerError(model.ErrGitHubAPIStatus, fmt.Sprintf("GitHub status %d: %s", resp.StatusCode, strings.TrimSpace(string(rawBody))), nil)
	}

	if remaining, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining"))); err == nil && remaining >= 0 && remaining <= 100 {
		slog.Default().Warn("GitHub API rate limit is low", "remaining", remaining, "reset", strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")))
	}

	if err := json.Unmarshal(rawBody, target); err != nil {
		return nil, model.NewTrackerError(model.ErrGitHubUnknownPayload, "decode GitHub payload", err)
	}

	return resp.Header.Clone(), nil
}

func (c *GitHubClient) currentConfig() *model.ServiceConfig {
	if c.configProvider == nil {
		return &model.ServiceConfig{}
	}
	cfg := c.configProvider()
	if cfg == nil {
		return &model.ServiceConfig{}
	}

	return cfg
}

func buildGitHubListURL(cfg *model.ServiceConfig, state string) string {
	normalizedState := model.NormalizeState(state)
	queryState := "open"
	labelValue := ""

	switch {
	case normalizedState == "closed":
		queryState = "closed"
	case containsNormalizedState(githubTerminalStates(cfg), normalizedState):
		queryState = "closed"
		labelValue = githubStateLabelPrefix(cfg) + normalizedState
	default:
		labelValue = githubStateLabelPrefix(cfg) + normalizedState
	}

	values := url.Values{}
	if labelValue != "" {
		values.Set("labels", labelValue)
	}
	values.Set("per_page", strconv.Itoa(defaultGitHubPageSize))
	values.Set("state", queryState)

	return fmt.Sprintf("%s/repos/%s/%s/issues?%s", githubEndpoint(cfg), url.PathEscape(strings.TrimSpace(cfg.TrackerOwner)), url.PathEscape(strings.TrimSpace(cfg.TrackerRepo)), values.Encode())
}

func normalizeGitHubIssue(cfg *model.ServiceConfig, item gitHubIssue) (model.Issue, bool) {
	issue := model.Issue{
		ID:         strconv.Itoa(item.Number),
		Identifier: fmt.Sprintf("%s/%s#%d", strings.TrimSpace(cfg.TrackerOwner), strings.TrimSpace(cfg.TrackerRepo), item.Number),
		Title:      strings.TrimSpace(item.Title),
		Labels:     normalizeGitHubLabels(item.Labels),
		CreatedAt:  parseTime(item.CreatedAt),
		UpdatedAt:  parseTime(item.UpdatedAt),
	}
	branchName := fmt.Sprintf("issue-%d", item.Number)
	issue.BranchName = stringPtr(branchName)

	if item.Body != nil {
		if body := strings.TrimSpace(*item.Body); body != "" {
			issue.Description = stringPtr(body)
		}
	}
	if htmlURL := strings.TrimSpace(item.HTMLURL); htmlURL != "" {
		issue.URL = stringPtr(htmlURL)
	}

	state, include := extractGitHubState(item.State, issue.Labels, githubStateLabelPrefix(cfg), githubTerminalStates(cfg))
	issue.State = state
	return issue, include
}

func normalizeGitHubLabels(labels []gitHubLabel) []string {
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if value := strings.ToLower(strings.TrimSpace(label.Name)); value != "" {
			result = append(result, value)
		}
	}

	return result
}

func extractGitHubState(issueState string, labels []string, prefix string, terminalStates []string) (string, bool) {
	normalizedState := model.NormalizeState(issueState)
	normalizedPrefix := strings.ToLower(strings.TrimSpace(prefix))
	if normalizedPrefix == "" {
		normalizedPrefix = defaultGitHubStateLabelPrefix
	}

	matches := make([]string, 0, len(labels))
	for _, label := range labels {
		if !strings.HasPrefix(label, normalizedPrefix) {
			continue
		}
		state := strings.TrimSpace(strings.TrimPrefix(label, normalizedPrefix))
		if state == "" {
			continue
		}
		matches = append(matches, state)
	}

	if normalizedState == "closed" {
		terminalSet := make(map[string]struct{}, len(terminalStates))
		for _, state := range terminalStates {
			if normalized := model.NormalizeState(state); normalized != "" {
				terminalSet[normalized] = struct{}{}
			}
		}
		for _, match := range matches {
			if _, ok := terminalSet[match]; ok {
				return match, true
			}
		}

		return "closed", true
	}

	if len(matches) == 0 {
		return "", false
	}
	if len(matches) > 1 {
		slog.Default().Warn("GitHub issue has multiple state labels and will be skipped", "labels", matches)
		return "", false
	}

	return matches[0], true
}

func githubEndpoint(cfg *model.ServiceConfig) string {
	if cfg == nil {
		return defaultGitHubEndpoint
	}
	if endpoint := strings.TrimRight(strings.TrimSpace(cfg.TrackerEndpoint), "/"); endpoint != "" {
		return endpoint
	}

	return defaultGitHubEndpoint
}

func githubStateLabelPrefix(cfg *model.ServiceConfig) string {
	if cfg == nil {
		return defaultGitHubStateLabelPrefix
	}
	if prefix := strings.ToLower(strings.TrimSpace(cfg.TrackerStateLabelPrefix)); prefix != "" {
		return prefix
	}

	return defaultGitHubStateLabelPrefix
}

func githubActiveStates(cfg *model.ServiceConfig) []string {
	states := normalizeGitHubStates(defaultGitHubActiveStates)
	if cfg == nil || len(cfg.ActiveStates) == 0 {
		return states
	}

	return normalizeGitHubStates(cfg.ActiveStates)
}

func githubTerminalStates(cfg *model.ServiceConfig) []string {
	states := normalizeGitHubStates(defaultGitHubTerminalStates)
	if cfg == nil || len(cfg.TerminalStates) == 0 {
		return states
	}

	return normalizeGitHubStates(cfg.TerminalStates)
}

func normalizeGitHubStates(states []string) []string {
	result := make([]string, 0, len(states))
	for _, state := range states {
		if normalized := model.NormalizeState(state); normalized != "" {
			result = append(result, normalized)
		}
	}

	return result
}

func containsNormalizedState(states []string, target string) bool {
	normalizedTarget := model.NormalizeState(target)
	for _, state := range states {
		if model.NormalizeState(state) == normalizedTarget {
			return true
		}
	}

	return false
}

func parseGitHubNextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}

		link := strings.TrimSpace(sections[0])
		rel := strings.TrimSpace(sections[1])
		if rel != `rel="next"` {
			continue
		}

		return strings.Trim(link, "<>")
	}

	return ""
}

func isGitHubRateLimited(headers http.Header, rawBody []byte) bool {
	if strings.TrimSpace(headers.Get("Retry-After")) != "" {
		return true
	}
	if strings.TrimSpace(headers.Get("X-RateLimit-Remaining")) == "0" {
		return true
	}

	return bytes.Contains(bytes.ToLower(rawBody), []byte("rate limit"))
}

func buildGitHubRateLimitMessage(resp *http.Response, rawBody []byte) string {
	parts := []string{"GitHub API rate limited"}
	if retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After")); retryAfter != "" {
		parts = append(parts, fmt.Sprintf("retry after %ss", retryAfter))
	}
	if reset := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); reset != "" {
		if parsed, err := strconv.ParseInt(reset, 10, 64); err == nil {
			parts = append(parts, fmt.Sprintf("reset at %s", time.Unix(parsed, 0).UTC().Format(time.RFC3339)))
		}
	}
	if body := strings.TrimSpace(string(rawBody)); body != "" {
		parts = append(parts, body)
	}

	return strings.Join(parts, "; ")
}

func stringPtr(value string) *string {
	copyValue := value
	return &copyValue
}

type gitHubIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        *string         `json:"body"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	Labels      []gitHubLabel   `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

type gitHubLabel struct {
	Name string `json:"name"`
}

type githubIssueNode = gitHubIssue
type githubLabelNode = gitHubLabel

func extractGitHubIssueState(node githubIssueNode, labels []string, cfg *model.ServiceConfig) (string, bool) {
	return extractGitHubState(node.State, labels, githubStateLabelPrefix(cfg), githubTerminalStates(cfg))
}
