package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestFetchCandidateIssuesMapsTransportFailure(t *testing.T) {
	client := newTestLinearClientWithHTTP(t, "http://linear.invalid", &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial tcp: boom")
		}),
	})
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, model.ErrLinearAPIRequest) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrLinearAPIRequest", err)
	}
}

func TestFetchCandidateIssuesMapsMalformedPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	_, err := client.FetchCandidateIssues(context.Background())
	if !errors.Is(err, model.ErrLinearUnknownPayload) {
		t.Fatalf("FetchCandidateIssues() error = %v, want ErrLinearUnknownPayload", err)
	}
}

func TestTransitionIssueSuccess(t *testing.T) {
	var sawStateQuery bool
	var sawMutation bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "IssueTeamStates"):
			sawStateQuery = true
			if body.Variables["id"] != "1" {
				t.Fatalf("state query id = %v, want 1", body.Variables["id"])
			}
			_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[{"id":"done-state","name":"Done","type":"completed"},{"id":"progress-state","name":"In Progress","type":"started"}]}}}}}`))
		case strings.Contains(body.Query, "MoveIssueToState"):
			sawMutation = true
			if body.Variables["id"] != "1" {
				t.Fatalf("mutation id = %v, want 1", body.Variables["id"])
			}
			if body.Variables["stateId"] != "done-state" {
				t.Fatalf("stateId = %v, want done-state", body.Variables["stateId"])
			}
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Fatalf("unexpected query: %s", body.Query)
		}
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	if err := client.TransitionIssue(context.Background(), "1", "Done"); err != nil {
		t.Fatalf("TransitionIssue() error = %v", err)
	}
	if !sawStateQuery || !sawMutation {
		t.Fatalf("stateQuery=%v mutation=%v, want both true", sawStateQuery, sawMutation)
	}
}

func TestTransitionIssueCompletedFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "IssueTeamStates"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[{"id":"review-state","name":"In Review","type":"started"},{"id":"completed-state","name":"Closed","type":"completed"}]}}}}}`))
		case strings.Contains(body.Query, "MoveIssueToState"):
			if body.Variables["stateId"] != "completed-state" {
				t.Fatalf("stateId = %v, want completed-state", body.Variables["stateId"])
			}
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Fatalf("unexpected query: %s", body.Query)
		}
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	if err := client.TransitionIssue(context.Background(), "1", "Done"); err != nil {
		t.Fatalf("TransitionIssue() error = %v", err)
	}
}

func TestTransitionIssueStateNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(body.Query, "IssueTeamStates") {
			t.Fatalf("unexpected query: %s", body.Query)
		}
		_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[{"id":"todo-state","name":"Todo","type":"unstarted"},{"id":"progress-state","name":"In Progress","type":"started"}]}}}}}`))
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	err := client.TransitionIssue(context.Background(), "1", "Done")
	if !errors.Is(err, model.ErrLinearStateNotFound) {
		t.Fatalf("TransitionIssue() error = %v, want ErrLinearStateNotFound", err)
	}
}

func TestTransitionIssueMutationFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequestBody(t, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "IssueTeamStates"):
			_, _ = w.Write([]byte(`{"data":{"issue":{"team":{"states":{"nodes":[{"id":"done-state","name":"Done","type":"completed"}]}}}}}`))
		case strings.Contains(body.Query, "MoveIssueToState"):
			_, _ = w.Write([]byte(`{"data":{"issueUpdate":{"success":false}}}`))
		default:
			t.Fatalf("unexpected query: %s", body.Query)
		}
	}))
	defer server.Close()

	client := newTestLinearClient(t, server.URL)
	err := client.TransitionIssue(context.Background(), "1", "Done")
	if !errors.Is(err, model.ErrLinearTransitionFailed) {
		t.Fatalf("TransitionIssue() error = %v, want ErrLinearTransitionFailed", err)
	}
}

func newTestLinearClient(t *testing.T, endpoint string) *LinearClient {
	t.Helper()

	return newTestLinearClientWithHTTP(t, endpoint, serverHTTPClient())
}

func newTestLinearClientWithHTTP(t *testing.T, endpoint string, httpClient *http.Client) *LinearClient {
	t.Helper()

	client, err := NewLinearClient(&model.ServiceConfig{
		TrackerEndpoint:    endpoint,
		TrackerAPIKey:      "secret",
		TrackerProjectSlug: "demo",
		ActiveStates:       []string{"Todo", "In Progress"},
	}, httpClient)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
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
