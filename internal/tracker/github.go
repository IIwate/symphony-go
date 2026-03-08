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
	"time"

	"symphony-go/internal/model"
)

const (
	githubDefaultPageSize = 100
	githubAcceptHeader    = "application/vnd.github+json"
	githubAPIVersion      = "2022-11-28"
	githubUserAgent       = "symphony-go"
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
	cfg := c.currentConfig()
	if len(cfg.ActiveStates) == 0 {
		return []model.Issue{}, nil
	}

	return c.fetchIssuesForStates(ctx, cfg.ActiveStates)
}

func (c *GitHubClient) FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error) {
	if len(states) == 0 {
		return []model.Issue{}, nil
	}

	return c.fetchIssuesForStates(ctx, states)
}

func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	normalized := normalizeUniqueStates(ids)
	if len(normalized) == 0 {
		return []model.Issue{}, nil
	}

	issues := make([]model.Issue, 0, len(normalized))
	for _, id := range normalized {
		item, err := c.requestIssue(ctx, c.issueURL(id))
		if err != nil {
			return nil, err
		}
		if item.PullRequest != nil {
			return nil, model.NewTrackerError(model.ErrGitHubPullRequest, fmt.Sprintf("github issue %s is a pull request", id), nil)
		}

		issue, ok := c.normalizeIssue(item)
		if !ok {
			continue
		}
		issues = append(issues, issue)
	}

	return issues, nil
}

func (c *GitHubClient) fetchIssuesForStates(ctx context.Context, states []string) ([]model.Issue, error) {
	requested := normalizeUniqueStates(states)
	if len(requested) == 0 {
		return []model.Issue{}, nil
	}

	requestedSet := make(map[string]struct{}, len(requested))
	for _, state := range requested {
		requestedSet[state] = struct{}{}
	}

	issuesByID := make(map[string]model.Issue)
	order := make([]string, 0)

	for _, state := range requested {
		nextURL := c.listIssuesURL(state)
		for nextURL != "" {
			page, followingURL, err := c.requestIssuePage(ctx, nextURL)
			if err != nil {
				return nil, err
			}
			for _, item := range page {
				issue, ok := c.normalizeIssue(item)
				if !ok {
					continue
				}
				if _, wanted := requestedSet[model.NormalizeState(issue.State)]; !wanted {
					continue
				}
				if _, exists := issuesByID[issue.ID]; exists {
					continue
				}
				order = append(order, issue.ID)
				issuesByID[issue.ID] = issue
			}
			nextURL = followingURL
		}
	}

	issues := make([]model.Issue, 0, len(order))
	for _, id := range order {
		issues = append(issues, issuesByID[id])
	}

	return issues, nil
}

func (c *GitHubClient) requestIssuePage(ctx context.Context, rawURL string) ([]githubIssue, string, error) {
	body, headers, err := c.doRequest(ctx, rawURL)
	if err != nil {
		return nil, "", err
	}

	var issues []githubIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return nil, "", model.NewTrackerError(model.ErrGitHubUnknownPayload, "decode GitHub issues payload", err)
	}

	return issues, parseNextLink(headers.Get("Link")), nil
}

func (c *GitHubClient) requestIssue(ctx context.Context, rawURL string) (githubIssue, error) {
	body, _, err := c.doRequest(ctx, rawURL)
	if err != nil {
		return githubIssue{}, err
	}

	var issue githubIssue
	if err := json.Unmarshal(body, &issue); err != nil {
		return githubIssue{}, model.NewTrackerError(model.ErrGitHubUnknownPayload, "decode GitHub issue payload", err)
	}

	return issue, nil
}

func (c *GitHubClient) doRequest(ctx context.Context, rawURL string) ([]byte, http.Header, error) {
	cfg := c.currentConfig()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "build GitHub request", err)
	}
	req.Header.Set("Accept", githubAcceptHeader)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.TrackerAPIKey))
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "execute GitHub request", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, model.NewTrackerError(model.ErrGitHubAPIRequest, "read GitHub response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, model.NewTrackerError(model.ErrGitHubAPIStatus, fmt.Sprintf("unexpected GitHub status %d", resp.StatusCode), nil)
	}

	return body, resp.Header.Clone(), nil
}

func (c *GitHubClient) listIssuesURL(state string) string {
	queryState, label := c.queryForState(state)

	values := url.Values{}
	values.Set("state", queryState)
	if label != "" {
		values.Set("labels", label)
	}
	values.Set("per_page", strconv.Itoa(githubDefaultPageSize))

	return c.issuesBaseURL() + "?" + values.Encode()
}

func (c *GitHubClient) issueURL(id string) string {
	return c.issuesBaseURL() + "/" + url.PathEscape(strings.TrimSpace(id))
}

func (c *GitHubClient) issuesBaseURL() string {
	cfg := c.currentConfig()
	return strings.TrimRight(strings.TrimSpace(cfg.TrackerEndpoint), "/") +
		"/repos/" + url.PathEscape(strings.TrimSpace(cfg.TrackerOwner)) +
		"/" + url.PathEscape(strings.TrimSpace(cfg.TrackerRepo)) +
		"/issues"
}

func (c *GitHubClient) queryForState(state string) (string, string) {
	normalized := model.NormalizeState(state)
	cfg := c.currentConfig()

	if normalized == "closed" {
		return "closed", ""
	}
	if containsNormalizedState(cfg.TerminalStates, normalized) {
		return "closed", c.stateLabel(normalized)
	}

	return "open", c.stateLabel(normalized)
}

func (c *GitHubClient) stateLabel(state string) string {
	return c.stateLabelPrefix() + model.NormalizeState(state)
}

func (c *GitHubClient) stateLabelPrefix() string {
	cfg := c.currentConfig()
	prefix := strings.TrimSpace(cfg.TrackerStateLabelPrefix)
	if prefix == "" {
		prefix = "symphony:"
	}

	return model.NormalizeState(prefix)
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

func (c *GitHubClient) normalizeIssue(item githubIssue) (model.Issue, bool) {
	if item.PullRequest != nil {
		return model.Issue{}, false
	}

	cfg := c.currentConfig()
	labels := normalizeGitHubLabels(item.Labels)
	state, ok := c.extractState(item, labels)
	if !ok {
		return model.Issue{}, false
	}

	issue := model.Issue{
		ID:         strconv.Itoa(item.Number),
		Identifier: fmt.Sprintf("%s/%s#%d", strings.TrimSpace(cfg.TrackerOwner), strings.TrimSpace(cfg.TrackerRepo), item.Number),
		Title:      item.Title,
		State:      state,
		Labels:     labels,
		CreatedAt:  parseTime(item.CreatedAt),
		UpdatedAt:  parseTime(item.UpdatedAt),
	}

	if text := strings.TrimSpace(item.Body); text != "" {
		issue.Description = &text
	}
	branchName := fmt.Sprintf("issue-%d", item.Number)
	issue.BranchName = &branchName
	if text := strings.TrimSpace(item.HTMLURL); text != "" {
		issue.URL = &text
	}

	return issue, true
}

func (c *GitHubClient) extractState(item githubIssue, labels []string) (string, bool) {
	if model.NormalizeState(item.State) == "closed" {
		matches := uniqueStates(filterTerminalStates(extractPrefixedStates(labels, c.stateLabelPrefix()), c.currentConfig().TerminalStates))
		if len(matches) > 1 {
			slog.Default().Warn("github issue has conflicting terminal state labels; skipping", "issue_number", item.Number, "states", strings.Join(matches, ","))
			return "", false
		}
		if len(matches) == 1 {
			return matches[0], true
		}

		return "closed", true
	}

	matches := uniqueStates(extractPrefixedStates(labels, c.stateLabelPrefix()))
	if len(matches) > 1 {
		slog.Default().Warn("github issue has conflicting state labels; skipping", "issue_number", item.Number, "states", strings.Join(matches, ","))
		return "", false
	}
	if len(matches) == 1 {
		return matches[0], true
	}

	return "", true
}

func normalizeGitHubLabels(labels []githubLabel) []string {
	if len(labels) == 0 {
		return nil
	}

	result := make([]string, 0, len(labels))
	for _, label := range labels {
		if value := model.NormalizeState(label.Name); value != "" {
			result = append(result, value)
		}
	}

	return result
}

func extractPrefixedStates(labels []string, prefix string) []string {
	states := make([]string, 0, len(labels))
	for _, label := range labels {
		normalized := model.NormalizeState(label)
		if !strings.HasPrefix(normalized, prefix) {
			continue
		}
		if state := model.NormalizeState(strings.TrimPrefix(normalized, prefix)); state != "" {
			states = append(states, state)
		}
	}

	return states
}

func filterTerminalStates(states []string, terminalStates []string) []string {
	filtered := make([]string, 0, len(states))
	for _, state := range states {
		if containsNormalizedState(terminalStates, state) {
			filtered = append(filtered, model.NormalizeState(state))
		}
	}

	return filtered
}

func uniqueStates(states []string) []string {
	seen := make(map[string]struct{}, len(states))
	result := make([]string, 0, len(states))
	for _, state := range states {
		normalized := model.NormalizeState(state)
		if normalized == "" {
			continue
		}
		if _, exists := seen[normalized]; exists {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}

	return result
}

func normalizeUniqueStates(states []string) []string {
	return uniqueStates(states)
}

func containsNormalizedState(states []string, target string) bool {
	normalized := model.NormalizeState(target)
	for _, state := range states {
		if model.NormalizeState(state) == normalized {
			return true
		}
	}

	return false
}

func parseNextLink(value string) string {
	for _, item := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		if len(parts) < 2 {
			continue
		}

		isNext := false
		for _, part := range parts[1:] {
			if strings.EqualFold(strings.TrimSpace(part), `rel="next"`) {
				isNext = true
				break
			}
		}
		if !isNext {
			continue
		}

		return strings.Trim(strings.TrimSpace(parts[0]), "<>")
	}

	return ""
}

type githubIssue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	HTMLURL     string          `json:"html_url"`
	State       string          `json:"state"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Labels      []githubLabel   `json:"labels"`
	PullRequest json.RawMessage `json:"pull_request"`
}

type githubLabel struct {
	Name string `json:"name"`
}
