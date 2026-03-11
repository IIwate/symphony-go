package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultGitHubAPIBaseURL = "https://api.github.com"

type gitHubPRLookup struct {
	httpClient  *http.Client
	apiBaseURL  string
	originURLFn func(context.Context, string) (string, error)
	ghAPIFn     func(context.Context, string, string) (string, error)
}

type gitHubRepo struct {
	Owner string
	Name  string
}

type gitHubHTTPStatusError struct {
	StatusCode int
}

func (e *gitHubHTTPStatusError) Error() string {
	return fmt.Sprintf("github api status %d", e.StatusCode)
}

func newGitHubPRLookup() PullRequestLookup {
	return &gitHubPRLookup{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		apiBaseURL: defaultGitHubAPIBaseURL,
		originURLFn: func(ctx context.Context, workspacePath string) (string, error) {
			stdout, stderr, err := runBashOutput(ctx, workspacePath, "git remote get-url origin")
			if err != nil {
				return "", fmt.Errorf("git remote get-url origin: %w: %s", err, strings.TrimSpace(stderr))
			}
			return strings.TrimSpace(stdout), nil
		},
		ghAPIFn: defaultGHAPIPulls,
	}
}

func (l *gitHubPRLookup) FindByHeadBranch(ctx context.Context, workspacePath string, headBranch string) (*PullRequestInfo, error) {
	branch := strings.TrimSpace(headBranch)
	if branch == "" {
		return nil, nil
	}
	if l == nil {
		return nil, errors.New("github pull request lookup is nil")
	}

	originURL := ""
	if l.originURLFn != nil {
		value, err := l.originURLFn(ctx, workspacePath)
		if err != nil {
			return nil, err
		}
		originURL = value
	}
	repo, err := parseGitHubRemoteURL(originURL)
	if err != nil {
		return nil, err
	}

	prs, err := l.lookupByREST(ctx, repo, branch)
	if err != nil {
		if isGitHubAuthStatus(err) && l.ghAPIFn != nil {
			fallback, fallbackErr := l.lookupByGHAPI(ctx, workspacePath, repo, branch)
			if fallbackErr == nil {
				return selectLatestPullRequest(branch, fallback), nil
			}
		}
		return nil, err
	}
	return selectLatestPullRequest(branch, prs), nil
}

func (l *gitHubPRLookup) lookupByREST(ctx context.Context, repo gitHubRepo, branch string) ([]PullRequestInfo, error) {
	client := l.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(l.apiBaseURL), "/")
	if baseURL == "" {
		baseURL = defaultGitHubAPIBaseURL
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		reqURL, err := url.Parse(baseURL + "/repos/" + url.PathEscape(repo.Owner) + "/" + url.PathEscape(repo.Name) + "/pulls")
		if err != nil {
			return nil, err
		}
		query := reqURL.Query()
		query.Set("state", "all")
		query.Set("head", repo.Owner+":"+branch)
		query.Set("per_page", "100")
		reqURL.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 2 && retryableGitHubLookupError(err) {
				time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
				continue
			}
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < 2 && retryableGitHubLookupError(readErr) {
				time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
				continue
			}
			return nil, readErr
		}
		if resp.StatusCode != http.StatusOK {
			statusErr := &gitHubHTTPStatusError{StatusCode: resp.StatusCode}
			lastErr = statusErr
			if attempt < 2 && retryableGitHubStatus(resp.StatusCode) {
				time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
				continue
			}
			return nil, statusErr
		}

		var payload []struct {
			Number   int     `json:"number"`
			HTMLURL  string  `json:"html_url"`
			State    string  `json:"state"`
			MergedAt *string `json:"merged_at"`
			Head     struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		result := make([]PullRequestInfo, 0, len(payload))
		for _, item := range payload {
			state := PullRequestStateClosed
			switch {
			case item.MergedAt != nil && strings.TrimSpace(*item.MergedAt) != "":
				state = PullRequestStateMerged
			case strings.EqualFold(item.State, "open"):
				state = PullRequestStateOpen
			}
			result = append(result, PullRequestInfo{
				Number:     item.Number,
				URL:        strings.TrimSpace(item.HTMLURL),
				HeadBranch: strings.TrimSpace(item.Head.Ref),
				State:      state,
			})
		}
		return result, nil
	}
	return nil, lastErr
}

func (l *gitHubPRLookup) lookupByGHAPI(ctx context.Context, workspacePath string, repo gitHubRepo, branch string) ([]PullRequestInfo, error) {
	if l.ghAPIFn == nil {
		return nil, errors.New("gh api fallback is not configured")
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls?state=all&head=%s:%s&per_page=100", repo.Owner, repo.Name, repo.Owner, branch)
	stdout, err := l.ghAPIFn(ctx, workspacePath, endpoint)
	if err != nil {
		return nil, err
	}

	var payload []struct {
		Number   int     `json:"number"`
		HTMLURL  string  `json:"html_url"`
		State    string  `json:"state"`
		MergedAt *string `json:"merged_at"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return nil, err
	}
	result := make([]PullRequestInfo, 0, len(payload))
	for _, item := range payload {
		state := PullRequestStateClosed
		switch {
		case item.MergedAt != nil && strings.TrimSpace(*item.MergedAt) != "":
			state = PullRequestStateMerged
		case strings.EqualFold(item.State, "open"):
			state = PullRequestStateOpen
		}
		result = append(result, PullRequestInfo{
			Number:     item.Number,
			URL:        strings.TrimSpace(item.HTMLURL),
			HeadBranch: strings.TrimSpace(item.Head.Ref),
			State:      state,
		})
	}
	return result, nil
}

func defaultGHAPIPulls(ctx context.Context, workspacePath string, endpoint string) (string, error) {
	stdout, stderr, err := runBashOutputWithTimeout(ctx, workspacePath, "gh api --method GET "+bashSingleQuote(endpoint), 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("gh api: %w: %s", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func parseGitHubRemoteURL(raw string) (gitHubRepo, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return gitHubRepo{}, errors.New("origin remote url is empty")
	}
	if strings.HasPrefix(trimmed, "git@github.com:") {
		return parseGitHubPath(strings.TrimPrefix(trimmed, "git@github.com:"))
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return gitHubRepo{}, err
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") {
		return gitHubRepo{}, fmt.Errorf("unsupported origin remote host %q", parsed.Host)
	}
	return parseGitHubPath(strings.TrimPrefix(parsed.Path, "/"))
}

func parseGitHubPath(raw string) (gitHubRepo, error) {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		return gitHubRepo{}, fmt.Errorf("unsupported origin remote path %q", raw)
	}
	if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return gitHubRepo{}, fmt.Errorf("unsupported origin remote path %q", raw)
	}
	return gitHubRepo{Owner: parts[0], Name: parts[1]}, nil
}

func selectLatestPullRequest(branch string, prs []PullRequestInfo) *PullRequestInfo {
	trimmedBranch := strings.TrimSpace(branch)
	var selected *PullRequestInfo
	for _, pr := range prs {
		if strings.TrimSpace(pr.HeadBranch) != trimmedBranch {
			continue
		}
		candidate := pr
		if selected == nil || candidate.Number > selected.Number {
			selected = &candidate
		}
	}
	return selected
}

func isGitHubAuthStatus(err error) bool {
	var statusErr *gitHubHTTPStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

func retryableGitHubStatus(statusCode int) bool {
	switch statusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryableGitHubLookupError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "tls handshake timeout") ||
		strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "eof")
}
