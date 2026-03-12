package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseGitHubRemoteURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    gitHubRepo
		wantErr bool
	}{
		{name: "https", raw: "https://github.com/IIwate/linear-test.git", want: gitHubRepo{Owner: "IIwate", Name: "linear-test"}},
		{name: "ssh scp", raw: "git@github.com:IIwate/linear-test.git", want: gitHubRepo{Owner: "IIwate", Name: "linear-test"}},
		{name: "ssh url", raw: "ssh://git@github.com/IIwate/linear-test.git", want: gitHubRepo{Owner: "IIwate", Name: "linear-test"}},
		{name: "other host", raw: "https://example.com/IIwate/linear-test.git", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGitHubRemoteURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGitHubRemoteURL() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHubRemoteURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseGitHubRemoteURL() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGitHubPRLookupFindByHeadBranchUsesREST(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "IIwate:feature/test" {
			t.Fatalf("head query = %q, want IIwate:feature/test", got)
		}
		_, _ = w.Write([]byte(`[{"number":41,"html_url":"https://example.test/pr/41","state":"open","merged_at":null,"head":{"ref":"feature/test","repo":{"owner":{"login":"IIwate"}}},"base":{"repo":{"name":"linear-test","owner":{"login":"IIwate"}}}}]`))
	}))
	defer server.Close()

	lookup := &gitHubPRLookup{
		httpClient: server.Client(),
		apiBaseURL: server.URL,
		remoteURLsFn: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"origin": "https://github.com/IIwate/linear-test.git"}, nil
		},
	}

	pr, err := lookup.FindByHeadBranch(context.Background(), "unused", "feature/test")
	if err != nil {
		t.Fatalf("FindByHeadBranch() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindByHeadBranch() = nil")
	}
	if pr.Number != 41 || pr.State != PullRequestStateOpen || pr.BaseOwner != "IIwate" || pr.BaseRepo != "linear-test" || pr.HeadOwner != "IIwate" {
		t.Fatalf("pull request = %+v", pr)
	}
}

func TestGitHubPRLookupFindByHeadBranchSupportsForkWorkspace(t *testing.T) {
	requests := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		if r.URL.Path != "/repos/upstream/linear-test/pulls" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if got := r.URL.Query().Get("head"); got != "fork-user:feature/test" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"number":57,"html_url":"https://example.test/pr/57","state":"open","merged_at":null,"head":{"ref":"feature/test","repo":{"owner":{"login":"fork-user"}}},"base":{"repo":{"name":"linear-test","owner":{"login":"upstream"}}}}]`))
	}))
	defer server.Close()

	lookup := &gitHubPRLookup{
		httpClient: server.Client(),
		apiBaseURL: server.URL,
		remoteURLsFn: func(context.Context, string) (map[string]string, error) {
			return map[string]string{
				"origin":   "https://github.com/fork-user/linear-test.git",
				"upstream": "https://github.com/upstream/linear-test.git",
			}, nil
		},
	}

	pr, err := lookup.FindByHeadBranch(context.Background(), "unused", "feature/test")
	if err != nil {
		t.Fatalf("FindByHeadBranch() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindByHeadBranch() = nil")
	}
	if pr.Number != 57 || pr.BaseOwner != "upstream" || pr.BaseRepo != "linear-test" || pr.HeadOwner != "fork-user" {
		t.Fatalf("pull request = %+v", pr)
	}
	if len(requests) == 0 {
		t.Fatal("expected at least one REST request")
	}
}

func TestGitHubPRLookupFallsBackToGHAPIOnForbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	lookup := &gitHubPRLookup{
		httpClient: server.Client(),
		apiBaseURL: server.URL,
		remoteURLsFn: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"origin": "https://github.com/IIwate/linear-test.git"}, nil
		},
		ghAPIFn: func(_ context.Context, _ string, endpoint string) (string, error) {
			want := "repos/IIwate/linear-test/pulls?state=all&head=IIwate:feature/test&per_page=100"
			if endpoint != want {
				return "", fmt.Errorf("endpoint = %q, want %q", endpoint, want)
			}
			return `[{"number":52,"html_url":"https://example.test/pr/52","state":"closed","merged_at":"2026-03-11T00:00:00Z","head":{"ref":"feature/test","repo":{"owner":{"login":"IIwate"}}},"base":{"repo":{"name":"linear-test","owner":{"login":"IIwate"}}}}]`, nil
		},
	}

	pr, err := lookup.FindByHeadBranch(context.Background(), "unused", "feature/test")
	if err != nil {
		t.Fatalf("FindByHeadBranch() error = %v", err)
	}
	if pr == nil {
		t.Fatal("FindByHeadBranch() = nil")
	}
	if pr.Number != 52 || pr.State != PullRequestStateMerged {
		t.Fatalf("pull request = %+v", pr)
	}
}

func TestGitHubPRLookupRefreshUsesStoredIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/upstream/linear-test/pulls/41" {
			t.Fatalf("path = %q, want /repos/upstream/linear-test/pulls/41", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":41,"html_url":"https://example.test/pr/41","state":"closed","merged_at":"2026-03-11T00:00:00Z","head":{"ref":"feature/test","repo":{"owner":{"login":"fork-user"}}},"base":{"repo":{"name":"linear-test","owner":{"login":"upstream"}}}}`))
	}))
	defer server.Close()

	lookup := &gitHubPRLookup{
		httpClient: server.Client(),
		apiBaseURL: server.URL,
	}

	pr, err := lookup.Refresh(context.Background(), "unused", &PullRequestInfo{
		Number:     41,
		HeadBranch: "feature/test",
		HeadOwner:  "fork-user",
		BaseOwner:  "upstream",
		BaseRepo:   "linear-test",
	})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if pr == nil {
		t.Fatal("Refresh() = nil")
	}
	if pr.State != PullRequestStateMerged || pr.BaseOwner != "upstream" || pr.HeadOwner != "fork-user" {
		t.Fatalf("pull request = %+v", pr)
	}
}
