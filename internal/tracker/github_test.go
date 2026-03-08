package tracker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"symphony-go/internal/model"
)

func TestNewClientRoutesGitHub(t *testing.T) {
	client, err := NewClient(&model.ServiceConfig{
		TrackerKind:   "github",
		TrackerAPIKey: "secret",
		TrackerOwner:  "octocat",
		TrackerRepo:   "hello-world",
	}, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, ok := client.(*GitHubClient); !ok {
		t.Fatalf("client = %T, want *GitHubClient", client)
	}
}

func TestGitHubClientAuthHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != githubAPIVersion {
			t.Fatalf("X-GitHub-Api-Version = %q, want %s", got, githubAPIVersion)
		}
		if got := r.URL.Path; got != "/repos/octocat/hello-world/issues/12" {
			t.Fatalf("Path = %q, want /repos/octocat/hello-world/issues/12", got)
		}
		_, _ = w.Write([]byte(`{"number":12,"title":"Issue","state":"open","labels":[{"name":"symphony:todo"}]}`))
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"12"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(issues) != 1 || issues[0].State != "todo" {
		t.Fatalf("issues = %+v, want todo", issues)
	}
}

func TestGitHubClientFetchCandidateIssues(t *testing.T) {
	var mu sync.Mutex
	callCount := map[string]int{}
	var server *httptest.Server

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		key := r.URL.Query().Get("labels") + "|" + r.URL.Query().Get("page")
		callCount[key]++
		mu.Unlock()

		if r.URL.Path != "/repos/octocat/hello-world/issues" {
			t.Fatalf("Path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Fatalf("state = %q, want open", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Fatalf("per_page = %q, want 100", got)
		}

		switch key {
		case "symphony:todo|":
			w.Header().Set("Link", "<"+server.URL+"/repos/octocat/hello-world/issues?state=open&labels=symphony%3Atodo&per_page=100&page=2>; rel=\"next\"")
			_, _ = w.Write([]byte(`[
				{"number":1,"title":"First","body":"desc","state":"open","html_url":"https://example.test/1","labels":[{"name":"symphony:todo"}],"created_at":"2026-03-07T00:00:00Z","updated_at":"2026-03-07T01:00:00Z"},
				{"number":99,"title":"Pull Request","state":"open","labels":[{"name":"symphony:todo"}],"pull_request":{}}
			]`))
		case "symphony:todo|2":
			_, _ = w.Write([]byte(`[
				{"number":2,"title":"Second","state":"open","labels":[{"name":"symphony:todo"}]}
			]`))
		case "symphony:in-progress|":
			_, _ = w.Write([]byte(`[
				{"number":3,"title":"Third","state":"open","labels":[{"name":"symphony:in-progress"}]}
			]`))
		default:
			t.Fatalf("unexpected query key %q", key)
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
	if issues[0].ID != "1" || issues[1].ID != "2" || issues[2].ID != "3" {
		t.Fatalf("issue order = %+v", issues)
	}
	if issues[0].Description == nil || *issues[0].Description != "desc" {
		t.Fatalf("Description = %+v, want desc", issues[0].Description)
	}
	if issues[0].URL == nil || *issues[0].URL != "https://example.test/1" {
		t.Fatalf("URL = %+v, want html_url", issues[0].URL)
	}
	if issues[0].CreatedAt == nil || issues[0].UpdatedAt == nil {
		t.Fatalf("timestamps not parsed: %+v", issues[0])
	}
}

func TestGitHubClientFetchIssuesByStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "closed" {
			t.Fatalf("state = %q, want closed", r.URL.Query().Get("state"))
		}
		switch r.URL.Query().Get("labels") {
		case "":
			_, _ = w.Write([]byte(`[
				{"number":4,"title":"Closed","state":"closed","labels":[]},
				{"number":5,"title":"Cancelled","state":"closed","labels":[{"name":"symphony:cancelled"}]}
			]`))
		case "symphony:cancelled":
			_, _ = w.Write([]byte(`[
				{"number":5,"title":"Cancelled","state":"closed","labels":[{"name":"symphony:cancelled"}]}
			]`))
		default:
			t.Fatalf("labels = %q", r.URL.Query().Get("labels"))
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
	if issues[0].State != "closed" || issues[1].State != "cancelled" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestGitHubClientFetchIssueStatesByIDsConcurrent(t *testing.T) {
	var current atomic.Int32
	var maxSeen atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value := current.Add(1)
		defer current.Add(-1)
		for {
			max := maxSeen.Load()
			if value <= max || maxSeen.CompareAndSwap(max, value) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		number := pathIssueNumber(r.URL.Path)
		_, _ = w.Write([]byte(`{"number":` + number + `,"title":"Issue","state":"open","labels":[{"name":"symphony:todo"}]}`))
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	ids := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}
	issues, err := client.FetchIssueStatesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(issues) != len(ids) {
		t.Fatalf("issues len = %d, want %d", len(issues), len(ids))
	}
	if maxSeen.Load() > githubRefreshConcurrency {
		t.Fatalf("max concurrency = %d, want <= %d", maxSeen.Load(), githubRefreshConcurrency)
	}
}

func TestGitHubClientFetchIssueStatesByIDsAllOrError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2") {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("boom"))
			return
		}
		number := pathIssueNumber(r.URL.Path)
		_, _ = w.Write([]byte(`{"number":` + number + `,"title":"Issue","state":"open","labels":[{"name":"symphony:todo"}]}`))
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1", "2", "3"})
	if !errors.Is(err, model.ErrGitHubAPIStatus) {
		t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrGitHubAPIStatus", err)
	}
	if issues != nil {
		t.Fatalf("issues = %+v, want nil on error", issues)
	}
}

func TestGitHubClientStateExtraction(t *testing.T) {
	client := newTestGitHubClient(t, "https://api.github.com")

	state, conflict := client.extractGitHubState([]string{"symphony:todo"}, "open")
	if state != "todo" || conflict {
		t.Fatalf("open state = %q, conflict = %v, want todo/false", state, conflict)
	}

	state, conflict = client.extractGitHubState([]string{"symphony:cancelled"}, "closed")
	if state != "cancelled" || conflict {
		t.Fatalf("closed state = %q, conflict = %v, want cancelled/false", state, conflict)
	}

	state, conflict = client.extractGitHubState(nil, "open")
	if state != "" || conflict {
		t.Fatalf("no labels state = %q, conflict = %v, want empty/false", state, conflict)
	}

	state, conflict = client.extractGitHubState([]string{"symphony:todo", "symphony:in-progress"}, "open")
	if state != "" || !conflict {
		t.Fatalf("conflict state = %q, conflict = %v, want empty/true", state, conflict)
	}
}

func TestGitHubClientUnexpectedPullRequestByID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":8,"title":"PR","state":"open","labels":[{"name":"symphony:todo"}],"pull_request":{}}`))
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	_, err := client.FetchIssueStatesByIDs(context.Background(), []string{"8"})
	if !errors.Is(err, model.ErrGitHubUnexpectedPullRequest) {
		t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrGitHubUnexpectedPullRequest", err)
	}
}

func TestGitHubClientRateLimit(t *testing.T) {
	t.Run("remaining zero", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1772935200")
			_, _ = w.Write([]byte(`{"number":1,"title":"Issue","state":"open","labels":[{"name":"symphony:todo"}]}`))
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL)
		_, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1"})
		if !errors.Is(err, model.ErrGitHubRateLimit) {
			t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrGitHubRateLimit", err)
		}
		if !strings.Contains(err.Error(), "retry at") {
			t.Fatalf("error = %v, want retry hint", err)
		}
	})

	t.Run("secondary rate limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL)
		_, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1"})
		if !errors.Is(err, model.ErrGitHubRateLimit) {
			t.Fatalf("FetchIssueStatesByIDs() error = %v, want ErrGitHubRateLimit", err)
		}
		if !strings.Contains(err.Error(), "retry after 60s") {
			t.Fatalf("error = %v, want Retry-After hint", err)
		}
	})

	t.Run("context done respected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		defer server.Close()

		client := newTestGitHubClient(t, server.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := client.FetchIssueStatesByIDs(ctx, []string{"1"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FetchIssueStatesByIDs() error = %v, want context deadline exceeded", err)
		}
	})
}

func TestGitHubClientWarnsOnLowRemaining(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previous)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "5")
		_, _ = w.Write([]byte(`{"number":1,"title":"Issue","state":"open","labels":[{"name":"symphony:todo"}]}`))
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	_, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if !strings.Contains(buf.String(), "github tracker rate limit remaining is low") {
		t.Fatalf("log output = %q, want low remaining warning", buf.String())
	}
}

func newTestGitHubClient(t *testing.T, endpoint string) *GitHubClient {
	t.Helper()

	client, err := NewGitHubClient(&model.ServiceConfig{
		TrackerEndpoint:         endpoint,
		TrackerAPIKey:           "secret",
		TrackerOwner:            "octocat",
		TrackerRepo:             "hello-world",
		TrackerStateLabelPrefix: "symphony:",
		ActiveStates:            []string{"todo", "in-progress"},
		TerminalStates:          []string{"closed", "cancelled"},
		PollIntervalMS:          30000,
	}, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewGitHubClient() error = %v", err)
	}

	return client
}

func pathIssueNumber(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return parts[len(parts)-1]
}
