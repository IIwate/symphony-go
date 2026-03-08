package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"symphony-go/internal/model"
)

func TestGitHubClient_FetchCandidateIssues(t *testing.T) {
	var server *httptest.Server
	var callCount int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		if r.URL.Path != "/repos/octo/hello/issues" {
			t.Fatalf("path = %s, want /repos/octo/hello/issues", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Fatalf("state = %q, want open", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Fatalf("per_page = %q, want 100", got)
		}

		label := r.URL.Query().Get("labels")
		page := r.URL.Query().Get("page")
		switch {
		case label == "symphony:todo" && page == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo/hello/issues?state=open&labels=symphony:todo&per_page=100&page=2>; rel="next"`, server.URL))
			writeGitHubJSON(t, w, []map[string]any{
				githubIssuePayload(1, "open", []string{"symphony:todo", "Bug"}),
				githubPullRequestPayload(999, "open", []string{"symphony:todo"}),
			})
		case label == "symphony:todo" && page == "2":
			writeGitHubJSON(t, w, []map[string]any{
				githubIssuePayload(2, "open", []string{"symphony:todo"}),
			})
		case label == "symphony:in-progress" && page == "":
			writeGitHubJSON(t, w, []map[string]any{
				githubIssuePayload(3, "open", []string{"symphony:in-progress", "ops"}),
			})
		default:
			t.Fatalf("unexpected request labels=%q page=%q", label, page)
		}
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("issues len = %d, want 3", len(issues))
	}
	if got := atomic.LoadInt32(&callCount); got != 3 {
		t.Fatalf("request count = %d, want 3", got)
	}
	if issues[0].ID != "1" || issues[0].Identifier != "octo/hello#1" {
		t.Fatalf("first issue = %+v", issues[0])
	}
	if issues[0].State != "todo" {
		t.Fatalf("first issue state = %q, want todo", issues[0].State)
	}
	if issues[0].BranchName == nil || *issues[0].BranchName != "issue-1" {
		t.Fatalf("first issue branch = %v, want issue-1", issues[0].BranchName)
	}
	if issues[0].Description == nil || *issues[0].Description != "body 1" {
		t.Fatalf("first issue description = %v", issues[0].Description)
	}
	if issues[0].URL == nil || *issues[0].URL != "https://github.com/octo/hello/issues/1" {
		t.Fatalf("first issue url = %v", issues[0].URL)
	}
	if issues[0].CreatedAt == nil || issues[0].UpdatedAt == nil {
		t.Fatalf("first issue timestamps = created:%v updated:%v", issues[0].CreatedAt, issues[0].UpdatedAt)
	}
	if len(issues[0].Labels) != 2 || issues[0].Labels[0] != "symphony:todo" || issues[0].Labels[1] != "bug" {
		t.Fatalf("first issue labels = %v", issues[0].Labels)
	}
}

func TestGitHubClient_FetchIssuesByStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("labels")
		state := r.URL.Query().Get("state")
		switch {
		case state == "closed" && label == "":
			writeGitHubJSON(t, w, []map[string]any{
				githubIssuePayload(10, "closed", nil),
				githubIssuePayload(11, "closed", []string{"symphony:cancelled"}),
				githubPullRequestPayload(12, "closed", []string{"symphony:cancelled"}),
			})
		case state == "closed" && label == "symphony:cancelled":
			writeGitHubJSON(t, w, []map[string]any{
				githubIssuePayload(11, "closed", []string{"symphony:cancelled"}),
			})
		default:
			t.Fatalf("unexpected request state=%q labels=%q", state, label)
		}
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	issues, err := client.FetchIssuesByStates(context.Background(), []string{"closed", "cancelled"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want 2", len(issues))
	}
	if issues[0].ID != "10" || issues[0].State != "closed" {
		t.Fatalf("first issue = %+v, want closed issue 10", issues[0])
	}
	if issues[1].ID != "11" || issues[1].State != "cancelled" {
		t.Fatalf("second issue = %+v, want cancelled issue 11", issues[1])
	}
}

func TestGitHubClient_FetchIssueStatesByIDs(t *testing.T) {
	t.Run("success uses concurrency", func(t *testing.T) {
		var current int32
		var maxConcurrent int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := atomic.AddInt32(&current, 1)
			for {
				seen := atomic.LoadInt32(&maxConcurrent)
				if started <= seen || atomic.CompareAndSwapInt32(&maxConcurrent, seen, started) {
					break
				}
			}
			defer atomic.AddInt32(&current, -1)
			time.Sleep(40 * time.Millisecond)

			switch r.URL.Path {
			case "/repos/octo/hello/issues/1":
				writeGitHubJSON(t, w, githubIssuePayload(1, "open", []string{"symphony:todo"}))
			case "/repos/octo/hello/issues/2":
				writeGitHubJSON(t, w, githubIssuePayload(2, "open", []string{"symphony:in-progress"}))
			case "/repos/octo/hello/issues/3":
				writeGitHubJSON(t, w, githubIssuePayload(3, "closed", nil))
			default:
				t.Fatalf("unexpected path %s", r.URL.Path)
			}
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL)
		issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1", "2", "3"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
		}
		if len(issues) != 3 {
			t.Fatalf("issues len = %d, want 3", len(issues))
		}
		if atomic.LoadInt32(&maxConcurrent) < 2 {
			t.Fatalf("max concurrent = %d, want at least 2", maxConcurrent)
		}
	})

	t.Run("fails whole call when one request fails", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/2") {
				w.WriteHeader(http.StatusBadGateway)
				writeGitHubJSON(t, w, map[string]any{"message": "upstream failed"})
				return
			}
			writeGitHubJSON(t, w, githubIssuePayload(1, "open", []string{"symphony:todo"}))
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL)
		issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1", "2"})
		if !errors.Is(err, model.ErrGitHubAPIStatus) {
			t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrGitHubAPIStatus", err)
		}
		if issues != nil {
			t.Fatalf("issues = %+v, want nil on error", issues)
		}
	})
}
func TestGitHubClient_StateExtraction(t *testing.T) {
	cfg := &model.ServiceConfig{
		TrackerOwner:            "octo",
		TrackerRepo:             "hello",
		TrackerStateLabelPrefix: "symphony:",
		TerminalStates:          []string{"closed", "cancelled"},
	}

	tests := []struct {
		name      string
		payload   gitHubIssue
		wantState string
		wantOK    bool
	}{
		{
			name:      "open issue with state label",
			payload:   mustGitHubIssue(1, "open", []string{"symphony:todo"}),
			wantState: "todo",
			wantOK:    true,
		},
		{
			name:      "closed issue with terminal label wins",
			payload:   mustGitHubIssue(2, "closed", []string{"symphony:cancelled", "bug"}),
			wantState: "cancelled",
			wantOK:    true,
		},
		{
			name:      "closed issue without terminal label falls back to closed",
			payload:   mustGitHubIssue(3, "closed", nil),
			wantState: "closed",
			wantOK:    true,
		},
		{
			name:    "open issue without matching label is skipped",
			payload: mustGitHubIssue(4, "open", []string{"bug"}),
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			issue, ok := normalizeGitHubIssue(cfg, tc.payload)
			if ok != tc.wantOK {
				t.Fatalf("normalizeGitHubIssue() ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if issue.State != tc.wantState {
				t.Fatalf("issue.State = %q, want %q", issue.State, tc.wantState)
			}
		})
	}
}

func TestGitHubClient_MultipleStateLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeGitHubJSON(t, w, []map[string]any{
			githubIssuePayload(1, "open", []string{"symphony:todo", "symphony:in-progress"}),
		})
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL, func(cfg *model.ServiceConfig) {
		cfg.ActiveStates = []string{"todo"}
	})
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues len = %d, want 0", len(issues))
	}
}

func TestGitHubClient_RateLimit(t *testing.T) {
	t.Run("429 returns retry hint", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			writeGitHubJSON(t, w, map[string]any{"message": "secondary rate limit"})
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL, func(cfg *model.ServiceConfig) {
			cfg.ActiveStates = []string{"todo"}
		})
		_, err := client.FetchCandidateIssues(context.Background())
		if !errors.Is(err, model.ErrGitHubRateLimited) {
			t.Fatalf("FetchCandidateIssues() error = %v, want ErrGitHubRateLimited", err)
		}
		if !strings.Contains(err.Error(), "retry after 60s") {
			t.Fatalf("rate limit error = %v, want Retry-After hint", err)
		}
	})

	t.Run("403 with reset header returns rate limit error", func(t *testing.T) {
		resetAt := time.Now().Add(2 * time.Minute).Unix()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))
			w.WriteHeader(http.StatusForbidden)
			writeGitHubJSON(t, w, map[string]any{"message": "API rate limit exceeded"})
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL, func(cfg *model.ServiceConfig) {
			cfg.ActiveStates = []string{"todo"}
		})
		_, err := client.FetchCandidateIssues(context.Background())
		if !errors.Is(err, model.ErrGitHubRateLimited) {
			t.Fatalf("FetchCandidateIssues() error = %v, want ErrGitHubRateLimit", err)
		}
		if !strings.Contains(err.Error(), "reset at") {
			t.Fatalf("rate limit error = %v, want reset hint", err)
		}
	})

	t.Run("cancelled context aborts request", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL, func(cfg *model.ServiceConfig) {
			cfg.ActiveStates = []string{"todo"}
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := client.FetchCandidateIssues(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("FetchCandidateIssues() error = %v, want context.Canceled", err)
		}
	})
}

func TestGitHubClient_Auth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("Accept = %q, want application/vnd.github+json", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Fatalf("X-GitHub-Api-Version = %q, want 2022-11-28", got)
		}
		writeGitHubJSON(t, w, []map[string]any{})
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL, func(cfg *model.ServiceConfig) {
		cfg.ActiveStates = []string{"todo"}
	})
	if _, err := client.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
}
func TestNewClient_GitHub(t *testing.T) {
	cfg := &model.ServiceConfig{
		TrackerKind:             "github",
		TrackerEndpoint:         "https://api.github.com",
		TrackerAPIKey:           "secret",
		TrackerOwner:            "octo",
		TrackerRepo:             "hello",
		TrackerStateLabelPrefix: "symphony:",
		ActiveStates:            []string{"todo", "in-progress"},
		TerminalStates:          []string{"closed", "cancelled"},
	}

	client, err := NewClient(cfg, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, ok := client.(*GitHubClient); !ok {
		t.Fatalf("NewClient() type = %T, want *GitHubClient", client)
	}

	dynamicClient, err := NewDynamicClient(func() *model.ServiceConfig { return cfg }, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewDynamicClient() error = %v", err)
	}
	if _, ok := dynamicClient.(*GitHubClient); !ok {
		t.Fatalf("NewDynamicClient() type = %T, want *GitHubClient", dynamicClient)
	}
}

func TestGitHubIntegration(t *testing.T) {
	if os.Getenv("SYMPHONY_GITHUB_INTEGRATION") != "1" {
		t.Skip("set SYMPHONY_GITHUB_INTEGRATION=1 to run GitHub integration tests")
	}

	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	owner := strings.TrimSpace(os.Getenv("GITHUB_TEST_OWNER"))
	repo := strings.TrimSpace(os.Getenv("GITHUB_TEST_REPO"))
	if token == "" || owner == "" || repo == "" {
		t.Fatalf("GITHUB_TOKEN, GITHUB_TEST_OWNER and GITHUB_TEST_REPO are required when SYMPHONY_GITHUB_INTEGRATION=1")
	}

	client, err := NewGitHubClient(&model.ServiceConfig{
		TrackerKind:             "github",
		TrackerAPIKey:           token,
		TrackerOwner:            owner,
		TrackerRepo:             repo,
		TrackerStateLabelPrefix: "symphony:",
		ActiveStates:            []string{"todo", "in-progress"},
		TerminalStates:          []string{"closed", "cancelled"},
	}, nil)
	if err != nil {
		t.Fatalf("NewGitHubClient() error = %v", err)
	}
	if _, err := client.FetchCandidateIssues(context.Background()); err != nil {
		t.Fatalf("FetchCandidateIssues() integration error = %v", err)
	}
}

func newTestGitHubClient(t *testing.T, endpoint string, mutations ...func(*model.ServiceConfig)) *GitHubClient {
	t.Helper()

	cfg := &model.ServiceConfig{
		TrackerKind:             "github",
		TrackerEndpoint:         endpoint,
		TrackerAPIKey:           "secret",
		TrackerOwner:            "octo",
		TrackerRepo:             "hello",
		TrackerStateLabelPrefix: "symphony:",
		ActiveStates:            []string{"todo", "in-progress"},
		TerminalStates:          []string{"closed", "cancelled"},
	}
	for _, mutate := range mutations {
		mutate(cfg)
	}

	client, err := NewGitHubClient(cfg, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewGitHubClient() error = %v", err)
	}
	return client
}

func writeGitHubJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func mustGitHubIssue(number int, issueState string, labels []string) gitHubIssue {
	payload := githubIssuePayload(number, issueState, labels)
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	var issue gitHubIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		panic(err)
	}
	return issue
}
func githubIssuePayload(number int, issueState string, labels []string) map[string]any {
	labelPayload := make([]map[string]any, 0, len(labels))
	for _, label := range labels {
		labelPayload = append(labelPayload, map[string]any{"name": label})
	}
	return map[string]any{
		"number":     number,
		"state":      issueState,
		"title":      fmt.Sprintf("Issue %d", number),
		"body":       fmt.Sprintf("body %d", number),
		"html_url":   fmt.Sprintf("https://github.com/octo/hello/issues/%d", number),
		"labels":     labelPayload,
		"created_at": "2026-03-07T00:00:00Z",
		"updated_at": "2026-03-07T01:00:00Z",
	}
}

func githubPullRequestPayload(number int, issueState string, labels []string) map[string]any {
	payload := githubIssuePayload(number, issueState, labels)
	payload["pull_request"] = map[string]any{"url": fmt.Sprintf("https://api.github.com/repos/octo/hello/pulls/%d", number)}
	return payload
}
