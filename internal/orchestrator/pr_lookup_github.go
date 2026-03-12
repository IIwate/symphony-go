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
	"sort"
	"strings"
	"time"
)

const defaultGitHubAPIBaseURL = "https://api.github.com"

type gitHubPRLookup struct {
	httpClient   *http.Client
	apiBaseURL   string
	remoteURLsFn func(context.Context, string) (map[string]string, error)
	ghAPIFn      func(context.Context, string, string) (string, error)
}

type gitHubRepo struct {
	Owner string
	Name  string
}

type gitHubHTTPStatusError struct {
	StatusCode int
}

type namedGitHubRepo struct {
	Remote string
	Repo   gitHubRepo
}

type pullRequestPayload struct {
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref  string `json:"ref"`
		Repo *struct {
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo *struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repo"`
	} `json:"base"`
}

func (e *gitHubHTTPStatusError) Error() string {
	return fmt.Sprintf("github api status %d", e.StatusCode)
}

func newGitHubPRLookup() PullRequestLookup {
	return &gitHubPRLookup{
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		apiBaseURL:   defaultGitHubAPIBaseURL,
		remoteURLsFn: defaultGitHubRemoteURLs,
		ghAPIFn:      defaultGHAPIPulls,
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

	remotes, err := l.gitHubRemotes(ctx, workspacePath)
	if err != nil {
		return nil, err
	}

	var lastErr error
	hadSuccessfulLookup := false
	for _, baseRepo := range orderedBaseRepos(remotes) {
		for _, headOwner := range orderedHeadOwners(remotes) {
			prs, lookupErr := l.lookupByHead(ctx, workspacePath, baseRepo, headOwner, branch)
			if lookupErr != nil {
				lastErr = lookupErr
				continue
			}
			hadSuccessfulLookup = true
			if pr := selectLatestPullRequest(branch, prs); pr != nil {
				return pr, nil
			}
		}
	}
	if hadSuccessfulLookup {
		return nil, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}

func (l *gitHubPRLookup) Refresh(ctx context.Context, workspacePath string, pr *PullRequestInfo) (*PullRequestInfo, error) {
	if pr == nil {
		return nil, nil
	}
	if l == nil {
		return nil, errors.New("github pull request lookup is nil")
	}
	if pr.Number > 0 && strings.TrimSpace(pr.BaseOwner) != "" && strings.TrimSpace(pr.BaseRepo) != "" {
		refreshed, err := l.lookupByNumber(ctx, workspacePath, gitHubRepo{
			Owner: strings.TrimSpace(pr.BaseOwner),
			Name:  strings.TrimSpace(pr.BaseRepo),
		}, pr.Number)
		if err != nil {
			return nil, err
		}
		if refreshed != nil {
			return refreshed, nil
		}
	}
	return l.FindByHeadBranch(ctx, workspacePath, pr.HeadBranch)
}

func (l *gitHubPRLookup) lookupByHead(ctx context.Context, workspacePath string, baseRepo gitHubRepo, headOwner string, branch string) ([]PullRequestInfo, error) {
	prs, err := l.lookupByREST(ctx, baseRepo, headOwner, branch)
	if err != nil {
		if isGitHubAuthStatus(err) && l.ghAPIFn != nil {
			fallback, fallbackErr := l.lookupByGHAPI(ctx, workspacePath, baseRepo, headOwner, branch)
			if fallbackErr == nil {
				return fallback, nil
			}
		}
		return nil, err
	}
	return prs, nil
}

func (l *gitHubPRLookup) lookupByNumber(ctx context.Context, workspacePath string, baseRepo gitHubRepo, number int) (*PullRequestInfo, error) {
	pr, err := l.lookupByNumberREST(ctx, baseRepo, number)
	if err != nil {
		if isGitHubAuthStatus(err) && l.ghAPIFn != nil {
			fallback, fallbackErr := l.lookupByNumberGHAPI(ctx, workspacePath, baseRepo, number)
			if fallbackErr == nil {
				return fallback, nil
			}
		}
		return nil, err
	}
	return pr, nil
}

func (l *gitHubPRLookup) lookupByREST(ctx context.Context, repo gitHubRepo, headOwner string, branch string) ([]PullRequestInfo, error) {
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
		query.Set("head", strings.TrimSpace(headOwner)+":"+branch)
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

		var payload []pullRequestPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		return decodePullRequestList(payload, repo, headOwner), nil
	}
	return nil, lastErr
}

func (l *gitHubPRLookup) lookupByNumberREST(ctx context.Context, repo gitHubRepo, number int) (*PullRequestInfo, error) {
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/pulls/%d", baseURL, url.PathEscape(repo.Owner), url.PathEscape(repo.Name), number), nil)
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
		if resp.StatusCode == http.StatusNotFound {
			return nil, &gitHubHTTPStatusError{StatusCode: resp.StatusCode}
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

		var payload pullRequestPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		pr := decodePullRequest(payload, repo, "")
		return &pr, nil
	}
	return nil, lastErr
}

func (l *gitHubPRLookup) lookupByGHAPI(ctx context.Context, workspacePath string, repo gitHubRepo, headOwner string, branch string) ([]PullRequestInfo, error) {
	if l.ghAPIFn == nil {
		return nil, errors.New("gh api fallback is not configured")
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls?state=all&head=%s:%s&per_page=100", repo.Owner, repo.Name, strings.TrimSpace(headOwner), branch)
	stdout, err := l.ghAPIFn(ctx, workspacePath, endpoint)
	if err != nil {
		return nil, err
	}

	var payload []pullRequestPayload
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return nil, err
	}
	return decodePullRequestList(payload, repo, headOwner), nil
}

func (l *gitHubPRLookup) lookupByNumberGHAPI(ctx context.Context, workspacePath string, repo gitHubRepo, number int) (*PullRequestInfo, error) {
	if l.ghAPIFn == nil {
		return nil, errors.New("gh api fallback is not configured")
	}
	endpoint := fmt.Sprintf("repos/%s/%s/pulls/%d", repo.Owner, repo.Name, number)
	stdout, err := l.ghAPIFn(ctx, workspacePath, endpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}

	var payload pullRequestPayload
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		return nil, err
	}
	pr := decodePullRequest(payload, repo, "")
	return &pr, nil
}

func defaultGHAPIPulls(ctx context.Context, workspacePath string, endpoint string) (string, error) {
	stdout, stderr, err := runBashOutputWithTimeout(ctx, workspacePath, "gh api --method GET "+bashSingleQuote(endpoint), 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("gh api: %w: %s", err, strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func defaultGitHubRemoteURLs(ctx context.Context, workspacePath string) (map[string]string, error) {
	stdout, stderr, err := runBashOutput(ctx, workspacePath, "git remote -v")
	if err != nil {
		return nil, fmt.Errorf("git remote -v: %w: %s", err, strings.TrimSpace(stderr))
	}

	remoteURLs := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 3 || fields[2] != "(fetch)" {
			continue
		}
		if _, exists := remoteURLs[fields[0]]; exists {
			continue
		}
		remoteURLs[fields[0]] = fields[1]
	}
	if len(remoteURLs) == 0 {
		return nil, errors.New("no git remotes found")
	}
	return remoteURLs, nil
}

func (l *gitHubPRLookup) gitHubRemotes(ctx context.Context, workspacePath string) ([]namedGitHubRepo, error) {
	if l.remoteURLsFn == nil {
		return nil, errors.New("github remote resolver is not configured")
	}
	rawRemotes, err := l.remoteURLsFn(ctx, workspacePath)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(rawRemotes))
	for name := range rawRemotes {
		names = append(names, name)
	}
	sort.Strings(names)

	result := make([]namedGitHubRepo, 0, len(names))
	for _, name := range names {
		repo, err := parseGitHubRemoteURL(rawRemotes[name])
		if err != nil {
			continue
		}
		result = append(result, namedGitHubRepo{Remote: name, Repo: repo})
	}
	if len(result) == 0 {
		return nil, errors.New("no GitHub remotes found")
	}
	return result, nil
}

func orderedBaseRepos(remotes []namedGitHubRepo) []gitHubRepo {
	return orderedUniqueRepos(remotes, []string{"upstream", "origin"})
}

func orderedHeadOwners(remotes []namedGitHubRepo) []string {
	preferred := orderedUniqueRepos(remotes, []string{"origin", "upstream"})
	owners := make([]string, 0, len(preferred))
	seen := map[string]struct{}{}
	for _, repo := range preferred {
		owner := strings.TrimSpace(repo.Owner)
		if owner == "" {
			continue
		}
		if _, exists := seen[owner]; exists {
			continue
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}
	return owners
}

func orderedUniqueRepos(remotes []namedGitHubRepo, preferred []string) []gitHubRepo {
	ordered := make([]namedGitHubRepo, 0, len(remotes))
	seenRemote := map[string]struct{}{}
	appendRemote := func(name string) {
		for _, remote := range remotes {
			if remote.Remote != name {
				continue
			}
			if _, exists := seenRemote[remote.Remote]; exists {
				return
			}
			seenRemote[remote.Remote] = struct{}{}
			ordered = append(ordered, remote)
			return
		}
	}
	for _, name := range preferred {
		appendRemote(name)
	}
	for _, remote := range remotes {
		appendRemote(remote.Remote)
	}

	result := make([]gitHubRepo, 0, len(ordered))
	seenRepo := map[string]struct{}{}
	for _, remote := range ordered {
		key := remote.Repo.Owner + "/" + remote.Repo.Name
		if _, exists := seenRepo[key]; exists {
			continue
		}
		seenRepo[key] = struct{}{}
		result = append(result, remote.Repo)
	}
	return result
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

func decodePullRequestList(payload []pullRequestPayload, fallbackBase gitHubRepo, fallbackHeadOwner string) []PullRequestInfo {
	result := make([]PullRequestInfo, 0, len(payload))
	for _, item := range payload {
		result = append(result, decodePullRequest(item, fallbackBase, fallbackHeadOwner))
	}
	return result
}

func decodePullRequest(payload pullRequestPayload, fallbackBase gitHubRepo, fallbackHeadOwner string) PullRequestInfo {
	state := PullRequestStateClosed
	switch {
	case payload.MergedAt != nil && strings.TrimSpace(*payload.MergedAt) != "":
		state = PullRequestStateMerged
	case strings.EqualFold(payload.State, "open"):
		state = PullRequestStateOpen
	}

	headOwner := strings.TrimSpace(fallbackHeadOwner)
	if payload.Head.Repo != nil {
		if value := strings.TrimSpace(payload.Head.Repo.Owner.Login); value != "" {
			headOwner = value
		}
	}

	baseOwner := strings.TrimSpace(fallbackBase.Owner)
	baseRepo := strings.TrimSpace(fallbackBase.Name)
	if payload.Base.Repo != nil {
		if value := strings.TrimSpace(payload.Base.Repo.Owner.Login); value != "" {
			baseOwner = value
		}
		if value := strings.TrimSpace(payload.Base.Repo.Name); value != "" {
			baseRepo = value
		}
	}

	return PullRequestInfo{
		Number:     payload.Number,
		URL:        strings.TrimSpace(payload.HTMLURL),
		HeadBranch: strings.TrimSpace(payload.Head.Ref),
		HeadOwner:  headOwner,
		BaseOwner:  baseOwner,
		BaseRepo:   baseRepo,
		State:      state,
	}
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
