package tracker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"symphony-go/internal/model"
)

func TestNewClientRoutesGitHub(t *testing.T) {
	cfg := &model.ServiceConfig{
		TrackerKind:             "github",
		TrackerEndpoint:         "https://api.github.com",
		TrackerAPIKey:           "secret",
		TrackerOwner:            "octo",
		TrackerRepo:             "demo",
		TrackerStateLabelPrefix: "symphony:",
	}

	client, err := NewClient(cfg, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if _, ok := client.(*GitHubClient); !ok {
		t.Fatalf("NewClient() = %T, want *GitHubClient", client)
	}

	dynamicClient, err := NewDynamicClient(func() *model.ServiceConfig { return cfg }, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewDynamicClient() error = %v", err)
	}
	if _, ok := dynamicClient.(*GitHubClient); !ok {
		t.Fatalf("NewDynamicClient() = %T, want *GitHubClient", dynamicClient)
	}
}

func TestGitHubClientFetchCandidateIssuesPaginatesDedupesAndNormalizes(t *testing.T) {
	var logBuffer bytes.Buffer
	originalLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	defer slog.SetDefault(originalLogger)

	requests := make([]string, 0)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("Authorization = %q, want Bearer secret", got)
		}
		if got := r.Header.Get("Accept"); got != githubAcceptHeader {
			t.Fatalf("Accept = %q, want %q", got, githubAcceptHeader)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != githubAPIVersion {
			t.Fatalf("X-GitHub-Api-Version = %q, want %q", got, githubAPIVersion)
		}

		requests = append(requests, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")

		labels := r.URL.Query().Get("labels")
		page := r.URL.Query().Get("page")

		switch {
		case labels == "symphony:todo" && page == "":
			w.Header().Set("Link", "<"+server.URL+"/repos/octo/demo/issues?state=open&labels=symphony%3Atodo&per_page=100&page=2>; rel=\"next\"")
			_, _ = io.WriteString(w, `[
				{
					"number": 101,
					"title": "Todo issue",
					"body": "  desc  ",
					"html_url": "https://example.com/issues/101",
					"state": "open",
					"created_at": "2026-03-07T00:00:00Z",
					"updated_at": "2026-03-07T01:00:00Z",
					"labels": [{"name": " Bug "}, {"name": " symphony:todo "}]
				},
				{
					"number": 999,
					"title": "PR item",
					"state": "open",
					"labels": [{"name": "symphony:todo"}],
					"pull_request": {"url": "https://example.com/pr/999"}
				}
			]`)
		case labels == "symphony:todo" && page == "2":
			_, _ = io.WriteString(w, `[
				{
					"number": 104,
					"title": "Conflict issue",
					"state": "open",
					"labels": [{"name": "symphony:todo"}, {"name": "symphony:in-progress"}]
				}
			]`)
		case labels == "symphony:in-progress":
			_, _ = io.WriteString(w, `[
				{
					"number": 101,
					"title": "Todo issue",
					"state": "open",
					"labels": [{"name": "symphony:todo"}]
				},
				{
					"number": 102,
					"title": "In Progress issue",
					"state": "open",
					"labels": [{"name": "symphony:in-progress"}]
				}
			]`)
		default:
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
	}))
	defer server.Close()

	client := newTestGitHubClient(t, server.URL)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want 2", len(issues))
	}
	if issues[0].ID != "101" || issues[0].Identifier != "octo/demo#101" {
		t.Fatalf("first issue = %+v", issues[0])
	}
	if issues[0].State != "todo" {
		t.Fatalf("first state = %q, want todo", issues[0].State)
	}
	if issues[0].Description == nil || *issues[0].Description != "desc" {
		t.Fatalf("first description = %v, want desc", issues[0].Description)
	}
	if issues[0].BranchName == nil || *issues[0].BranchName != "issue-101" {
		t.Fatalf("first branch = %v, want issue-101", issues[0].BranchName)
	}
	if issues[0].URL == nil || *issues[0].URL != "https://example.com/issues/101" {
		t.Fatalf("first url = %v, want issue url", issues[0].URL)
	}
	if issues[0].CreatedAt == nil || issues[0].UpdatedAt == nil {
		t.Fatalf("first timestamps not parsed: %+v", issues[0])
	}
	if got := strings.Join(issues[0].Labels, ","); got != "bug,symphony:todo" {
		t.Fatalf("first labels = %q, want bug,symphony:todo", got)
	}
	if issues[0].Priority != nil {
		t.Fatalf("first priority = %v, want nil", issues[0].Priority)
	}
	if len(issues[0].BlockedBy) != 0 {
		t.Fatalf("first blockedBy = %+v, want empty", issues[0].BlockedBy)
	}
	if issues[1].ID != "102" || issues[1].State != "in-progress" {
		t.Fatalf("second issue = %+v", issues[1])
	}
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	if !strings.Contains(logBuffer.String(), "conflicting state labels") {
		t.Fatalf("log output = %q, want conflict warning", logBuffer.String())
	}
}

func TestGitHubClientFetchIssuesByStatesSupportsTerminalMappings(t *testing.T) {
	requests := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")

		labels := r.URL.Query().Get("labels")
		state := r.URL.Query().Get("state")
		if state != "closed" {
			t.Fatalf("state = %q, want closed", state)
		}

		switch labels {
		case "":
			_, _ = io.WriteString(w, `[
				{
					"number": 201,
					"title": "Closed issue",
					"state": "closed",
					"labels": [{"name": "bug"}]
				},
				{
					"number": 202,
					"title": "Cancelled issue",
					"state": "closed",
					"labels": [{"name": "symphony:cancelled"}]
				}
			]`)
		case "symphony:cancelled":
			_, _ = io.WriteString(w, `[
				{
					"number": 202,
					"title": "Cancelled issue",
					"state": "closed",
					"labels": [{"name": "symphony:cancelled"}]
				}
			]`)
		default:
			t.Fatalf("unexpected labels = %q", labels)
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
	if issues[0].ID != "201" || issues[0].State != "closed" {
		t.Fatalf("first issue = %+v", issues[0])
	}
	if issues[1].ID != "202" || issues[1].State != "cancelled" {
		t.Fatalf("second issue = %+v", issues[1])
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
}

func newTestGitHubClient(t *testing.T, endpoint string) *GitHubClient {
	t.Helper()

	client, err := NewGitHubClient(&model.ServiceConfig{
		TrackerEndpoint:         endpoint,
		TrackerAPIKey:           "secret",
		TrackerOwner:            "octo",
		TrackerRepo:             "demo",
		TrackerStateLabelPrefix: "symphony:",
		ActiveStates:            []string{"todo", "in-progress"},
		TerminalStates:          []string{"closed", "cancelled"},
	}, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewGitHubClient() error = %v", err)
	}

	return client
}
