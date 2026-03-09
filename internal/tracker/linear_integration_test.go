//go:build integration

package tracker

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"symphony-go/internal/config"
	"symphony-go/internal/model"
	"symphony-go/internal/testutil"
	"symphony-go/internal/workflow"
)

func TestLinearIntegration_FetchCandidates(t *testing.T) {
	cfg := integrationLinearConfig(t)
	transport := &countingTransport{base: http.DefaultTransport}
	client, err := NewLinearClient(cfg, &http.Client{Timeout: 30 * time.Second, Transport: transport})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) == 0 {
		t.Skip("no candidate issues in configured project")
	}

	for _, issue := range issues {
		if strings.TrimSpace(issue.ID) == "" {
			t.Fatalf("issue id is empty: %+v", issue)
		}
		if strings.TrimSpace(issue.Identifier) == "" {
			t.Fatalf("issue identifier is empty: %+v", issue)
		}
		if strings.TrimSpace(issue.Title) == "" {
			t.Fatalf("issue title is empty: %+v", issue)
		}
		for _, label := range issue.Labels {
			if label != strings.ToLower(label) {
				t.Fatalf("label %q is not normalized to lowercase", label)
			}
		}
	}

	if len(issues) > defaultPageSize && transport.requestCount() < 2 {
		t.Fatalf("expected paginated live fetch for %d issues, request count = %d", len(issues), transport.requestCount())
	}
}

func TestLinearIntegration_FetchByIDs(t *testing.T) {
	cfg := integrationLinearConfig(t)
	client, err := NewLinearClient(cfg, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	candidates, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(candidates) == 0 {
		t.Skip("no candidate issues in configured project")
	}

	limit := min(3, len(candidates))
	ids := make([]string, 0, limit)
	expectedIdentifiers := make(map[string]string, limit)
	for _, issue := range candidates[:limit] {
		ids = append(ids, issue.ID)
		expectedIdentifiers[issue.ID] = issue.Identifier
	}

	issues, err := client.FetchIssueStatesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(issues) != len(ids) {
		t.Fatalf("issue count = %d, want %d", len(issues), len(ids))
	}

	for _, issue := range issues {
		expectedIdentifier, ok := expectedIdentifiers[issue.ID]
		if !ok {
			t.Fatalf("unexpected issue returned: %+v", issue)
		}
		if strings.TrimSpace(issue.Identifier) == "" {
			t.Fatalf("issue identifier is empty: %+v", issue)
		}
		if strings.TrimSpace(issue.State) == "" {
			t.Fatalf("issue state is empty: %+v", issue)
		}
		if issue.Identifier != expectedIdentifier {
			t.Fatalf("issue identifier = %q, want %q", issue.Identifier, expectedIdentifier)
		}
	}
}

func integrationLinearConfig(t *testing.T) *model.ServiceConfig {
	t.Helper()

	_ = testutil.RequireEnv(t, "LINEAR_API_KEY")

	definition, err := workflow.Load(repoWorkflowPath(t))
	if err != nil {
		t.Fatalf("workflow.Load() error = %v", err)
	}
	cfg, err := config.NewFromWorkflow(definition)
	if err != nil {
		t.Fatalf("config.NewFromWorkflow() error = %v", err)
	}
	if projectSlug := strings.TrimSpace(os.Getenv("LINEAR_PROJECT_SLUG")); projectSlug != "" {
		cfg.TrackerProjectSlug = projectSlug
	}
	return cfg
}

func repoWorkflowPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "WORKFLOW.md"))
}

type countingTransport struct {
	base  http.RoundTripper
	count int32
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	atomic.AddInt32(&t.count, 1)
	return base.RoundTrip(req)
}

func (t *countingTransport) requestCount() int {
	return int(atomic.LoadInt32(&t.count))
}

func min(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
