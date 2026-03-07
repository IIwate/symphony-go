package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"symphony-go/internal/model"
)

func TestFetchIssuesByStatesEmptySkipsRequest(t *testing.T) {
	var count int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	issues, err := client.FetchIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues len = %d, want 0", len(issues))
	}
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Fatalf("request count = %d, want 0", got)
	}
}

func TestFetchCandidateIssuesPaginatesAndNormalizes(t *testing.T) {
	var callCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body := decodeRequestBody(t, r.Body)
		if !strings.Contains(body.Query, "slugId") {
			t.Fatalf("query missing slugId filter: %s", body.Query)
		}
		if body.Variables["projectSlug"] != "demo" {
			t.Fatalf("projectSlug = %v, want demo", body.Variables["projectSlug"])
		}

		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"nodes": [
							{
								"id": "1",
								"identifier": "ABC-1",
								"title": "First",
								"description": "desc",
								"priority": 1,
								"createdAt": "2026-03-07T00:00:00Z",
								"updatedAt": "2026-03-07T01:00:00Z",
								"state": {"name": "Todo"},
								"labels": {"nodes": [{"name": "Bug"}]},
								"inverseRelations": {"nodes": [{"type": "blocks", "issue": {"id": "9", "identifier": "XYZ-9", "state": {"name": "In Progress"}}}]}
							}
						],
						"pageInfo": {"hasNextPage": true, "endCursor": "cursor-1"}
					}
				}
			}`))
			return
		}
		if body.Variables["after"] != "cursor-1" {
			t.Fatalf("after = %v, want cursor-1", body.Variables["after"])
		}
		_, _ = w.Write([]byte(`{
			"data": {
				"issues": {
					"nodes": [
						{
							"id": "2",
							"identifier": "ABC-2",
							"title": "Second",
							"state": {"name": "In Progress"},
							"labels": {"nodes": [{"name": "Feature"}]},
							"inverseRelations": {"nodes": []}
						}
					],
					"pageInfo": {"hasNextPage": false, "endCursor": null}
				}
			}
		}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	issues, err := client.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues() error = %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want 2", len(issues))
	}
	if issues[0].Identifier != "ABC-1" || issues[1].Identifier != "ABC-2" {
		t.Fatalf("issue order = %+v", issues)
	}
	if issues[0].Labels[0] != "bug" {
		t.Fatalf("labels = %+v, want lowercase", issues[0].Labels)
	}
	if len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0].Identifier == nil || *issues[0].BlockedBy[0].Identifier != "XYZ-9" {
		t.Fatalf("blockedBy = %+v", issues[0].BlockedBy)
	}
	if issues[0].CreatedAt == nil || issues[0].UpdatedAt == nil {
		t.Fatalf("createdAt/updatedAt not parsed: %+v", issues[0])
	}
}

func TestFetchIssueStatesByIDsUsesIDType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r.Body)
		if !strings.Contains(body.Query, "[ID!]") {
			t.Fatalf("query missing [ID!] type: %s", body.Query)
		}
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"1","identifier":"ABC-1","title":"Issue","state":{"name":"Todo"}}],"pageInfo":{"hasNextPage":false}}}}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	issues, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs() error = %v", err)
	}
	if len(issues) != 1 || issues[0].State != "Todo" {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestFetchCandidateIssuesMapsGraphQLErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"bad query"}]}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, model.ErrLinearGraphQLErrors) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrLinearGraphQLErrors", err)
	}
}

func TestFetchCandidateIssuesMissingEndCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":null}}}}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, model.ErrLinearMissingEndCursor) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrLinearMissingEndCursor", err)
	}
}

func TestFetchCandidateIssuesMapsHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, model.ErrLinearAPIStatus) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrLinearAPIStatus", err)
	}
}

func newTestLinearClient(t *testing.T, endpoint string) *LinearClient {
	t.Helper()

	client, err := NewLinearClient(&model.ServiceConfig{
		TrackerEndpoint:    endpoint,
		TrackerAPIKey:      "secret",
		TrackerProjectSlug: "demo",
		ActiveStates:       []string{"Todo", "In Progress"},
	}, serverHTTPClient())
	if err != nil {
		t.Fatalf("NewLinearClient() error = %v", err)
	}

	return client
}

func serverHTTPClient() *http.Client {
	return &http.Client{}
}

type capturedRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func decodeRequestBody(t *testing.T, body io.ReadCloser) capturedRequest {
	t.Helper()
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	var request capturedRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatalf("Unmarshal() error = %v, body = %s", err, string(raw))
	}

	return request
}
