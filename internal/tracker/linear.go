package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"symphony-go/internal/model"
)

const defaultPageSize = 50

const candidateIssuesQuery = `query CandidateIssues($projectSlug: String!, $states: [String!], $after: String, $first: Int!) {
  issues(
    first: $first,
    after: $after,
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state { name }
      labels { nodes { name } }
      inverseRelations { nodes { type issue { id identifier state { name } } } }
      children { nodes { id identifier state { name } } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const issuesByStatesQuery = `query IssuesByStates($projectSlug: String!, $states: [String!], $after: String, $first: Int!) {
  issues(
    first: $first,
    after: $after,
    filter: {
      project: { slugId: { eq: $projectSlug } }
      state: { name: { in: $states } }
    }
  ) {
    nodes {
      id
      identifier
      title
      description
      priority
      branchName
      url
      createdAt
      updatedAt
      state { name }
      labels { nodes { name } }
      inverseRelations { nodes { type issue { id identifier state { name } } } }
      children { nodes { id identifier state { name } } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const issueStatesByIDsQuery = `query IssueStatesByIDs($ids: [ID!]!, $after: String, $first: Int!) {
  issues(
    first: $first,
    after: $after,
    filter: {
      id: { in: $ids }
    }
  ) {
    nodes {
      id
      identifier
      title
      state { name }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const issueTeamStatesQuery = `query IssueTeamStates($id: String!) {
  issue(id: $id) {
    team {
      states {
        nodes {
          id
          name
          type
        }
      }
    }
  }
}`

const issueUpdateStateMutation = `mutation MoveIssueToState($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) {
    success
  }
}`

type Client interface {
	FetchCandidateIssues(ctx context.Context) ([]model.Issue, error)
	FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error)
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error)
}

type IssueTransitioner interface {
	TransitionIssue(ctx context.Context, issueID string, targetState string) error
}

type LinearClient struct {
	httpClient     *http.Client
	configProvider func() *model.ServiceConfig
}

func NewClient(cfg *model.ServiceConfig, httpClient *http.Client) (Client, error) {
	if cfg == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config is nil", nil)
	}
	if cfg.TrackerKind != "linear" {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.TrackerKind), nil)
	}

	return NewLinearClient(cfg, httpClient)
}

func NewLinearClient(cfg *model.ServiceConfig, httpClient *http.Client) (*LinearClient, error) {
	return NewDynamicLinearClient(func() *model.ServiceConfig { return cfg }, httpClient)
}

func NewDynamicClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (Client, error) {
	return NewDynamicLinearClient(configProvider, httpClient)
}

func NewDynamicLinearClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (*LinearClient, error) {
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
	if strings.TrimSpace(cfg.TrackerProjectSlug) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerProjectSlug, "tracker.project_slug is required", nil)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &LinearClient{
		httpClient:     httpClient,
		configProvider: configProvider,
	}, nil
}

func (c *LinearClient) FetchCandidateIssues(ctx context.Context) ([]model.Issue, error) {
	cfg := c.currentConfig()
	return c.fetchIssues(ctx, candidateIssuesQuery, map[string]any{
		"projectSlug": cfg.TrackerProjectSlug,
		"states":      append([]string(nil), cfg.ActiveStates...),
	})
}

func (c *LinearClient) FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error) {
	cfg := c.currentConfig()
	if len(states) == 0 {
		return []model.Issue{}, nil
	}

	return c.fetchIssues(ctx, issuesByStatesQuery, map[string]any{
		"projectSlug": cfg.TrackerProjectSlug,
		"states":      states,
	})
}

func (c *LinearClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	if len(ids) == 0 {
		return []model.Issue{}, nil
	}

	return c.fetchIssues(ctx, issueStatesByIDsQuery, map[string]any{
		"ids": ids,
	})
}

func (c *LinearClient) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	rawPayload, err := c.executeGraphQL(ctx, issueTeamStatesQuery, map[string]any{
		"id": issueID,
	})
	if err != nil {
		return err
	}

	var statePayload issueTeamStatesPayload
	if err := json.Unmarshal(rawPayload, &statePayload); err != nil {
		return model.NewTrackerError(model.ErrLinearUnknownPayload, "decode issue team states payload", err)
	}
	if statePayload.Issue == nil || statePayload.Issue.Team == nil || statePayload.Issue.Team.States == nil {
		return model.NewTrackerError(model.ErrLinearUnknownPayload, "issue team states payload is missing", nil)
	}

	stateID := ""
	normalizedTarget := model.NormalizeState(targetState)
	for _, state := range statePayload.Issue.Team.States.Nodes {
		if strings.TrimSpace(state.ID) == "" {
			continue
		}
		if model.NormalizeState(state.Name) == normalizedTarget {
			stateID = state.ID
			break
		}
	}
	if stateID == "" {
		for _, state := range statePayload.Issue.Team.States.Nodes {
			if strings.TrimSpace(state.ID) == "" {
				continue
			}
			if model.NormalizeState(state.Type) == "completed" {
				stateID = state.ID
				break
			}
		}
	}
	if stateID == "" {
		return model.NewTrackerError(model.ErrLinearStateNotFound, fmt.Sprintf("no Linear workflow state found for target %q", targetState), nil)
	}

	rawPayload, err = c.executeGraphQL(ctx, issueUpdateStateMutation, map[string]any{
		"id":      issueID,
		"stateId": stateID,
	})
	if err != nil {
		return err
	}

	var updatePayload issueUpdatePayload
	if err := json.Unmarshal(rawPayload, &updatePayload); err != nil {
		return model.NewTrackerError(model.ErrLinearUnknownPayload, "decode issue update payload", err)
	}
	if updatePayload.IssueUpdate == nil {
		return model.NewTrackerError(model.ErrLinearUnknownPayload, "issueUpdate payload is missing", nil)
	}
	if !updatePayload.IssueUpdate.Success {
		return model.NewTrackerError(model.ErrLinearTransitionFailed, "Linear issue transition returned success=false", nil)
	}

	return nil
}

func (c *LinearClient) fetchIssues(ctx context.Context, query string, baseVariables map[string]any) ([]model.Issue, error) {
	issues := make([]model.Issue, 0)
	var after *string

	for {
		variables := cloneVariables(baseVariables)
		variables["after"] = after
		variables["first"] = defaultPageSize

		connection, err := c.requestIssues(ctx, query, variables)
		if err != nil {
			return nil, err
		}

		cfg := c.currentConfig()
		for _, node := range connection.Nodes {
			issues = append(issues, normalizeIssue(node, cfg))
		}

		if !connection.PageInfo.HasNextPage {
			return issues, nil
		}
		if connection.PageInfo.EndCursor == nil || strings.TrimSpace(*connection.PageInfo.EndCursor) == "" {
			return nil, model.NewTrackerError(model.ErrLinearMissingEndCursor, "pageInfo.hasNextPage=true but endCursor is missing", nil)
		}
		after = connection.PageInfo.EndCursor
	}
}

func (c *LinearClient) executeGraphQL(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: variables})
	if err != nil {
		return nil, model.NewTrackerError(model.ErrLinearUnknownPayload, "marshal GraphQL request", err)
	}

	cfg := c.currentConfig()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TrackerEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, model.NewTrackerError(model.ErrLinearAPIRequest, "build Linear request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", cfg.TrackerAPIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrLinearAPIRequest, "execute Linear request", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, model.NewTrackerError(model.ErrLinearAPIRequest, "read Linear response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, model.NewTrackerError(model.ErrLinearAPIStatus, fmt.Sprintf("unexpected Linear status %d", resp.StatusCode), nil)
	}

	var envelope graphQLResponse
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return nil, model.NewTrackerError(model.ErrLinearUnknownPayload, "decode GraphQL envelope", err)
	}
	if len(envelope.Errors) > 0 {
		return nil, model.NewTrackerError(model.ErrLinearGraphQLErrors, joinGraphQLErrors(envelope.Errors), nil)
	}
	if len(envelope.Data) == 0 {
		return nil, model.NewTrackerError(model.ErrLinearUnknownPayload, "GraphQL data is missing", nil)
	}

	return envelope.Data, nil
}

func (c *LinearClient) requestIssues(ctx context.Context, query string, variables map[string]any) (*issueConnection, error) {
	rawPayload, err := c.executeGraphQL(ctx, query, variables)
	if err != nil {
		return nil, err
	}

	var payload issuesPayload
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		return nil, model.NewTrackerError(model.ErrLinearUnknownPayload, "decode issues payload", err)
	}
	if payload.Issues == nil {
		return nil, model.NewTrackerError(model.ErrLinearUnknownPayload, "issues payload is missing", nil)
	}

	return payload.Issues, nil
}

func cloneVariables(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source)+2)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func (c *LinearClient) currentConfig() *model.ServiceConfig {
	if c.configProvider == nil {
		return &model.ServiceConfig{}
	}
	cfg := c.configProvider()
	if cfg == nil {
		return &model.ServiceConfig{}
	}
	return cfg
}

func normalizeIssue(node issueNode, cfg *model.ServiceConfig) model.Issue {
	blockedBy := normalizeBlockers(node.InverseRelations)
	if cfg != nil && cfg.TrackerLinearChildrenBlockParent {
		blockedBy = append(blockedBy, normalizeChildrenAsBlockers(node.Children, cfg.TerminalStates)...)
	}
	issue := model.Issue{
		ID:         node.ID,
		Identifier: node.Identifier,
		Title:      node.Title,
		State:      node.State.Name,
		Labels:     normalizeLabels(node.Labels),
		BlockedBy:  blockedBy,
		Priority:   normalizePriority(node.Priority),
		CreatedAt:  parseTime(node.CreatedAt),
		UpdatedAt:  parseTime(node.UpdatedAt),
	}

	if text := strings.TrimSpace(node.Description); text != "" {
		issue.Description = &text
	}
	if text := strings.TrimSpace(node.BranchName); text != "" {
		issue.BranchName = &text
	}
	if text := strings.TrimSpace(node.URL); text != "" {
		issue.URL = &text
	}

	return issue
}

func normalizeLabels(connection *labelConnection) []string {
	if connection == nil {
		return nil
	}
	labels := make([]string, 0, len(connection.Nodes))
	for _, node := range connection.Nodes {
		if value := strings.ToLower(strings.TrimSpace(node.Name)); value != "" {
			labels = append(labels, value)
		}
	}

	return labels
}

func normalizeBlockers(connection *inverseRelationConnection) []model.BlockerRef {
	if connection == nil {
		return nil
	}
	blockers := make([]model.BlockerRef, 0, len(connection.Nodes))
	for _, node := range connection.Nodes {
		if strings.TrimSpace(node.Type) != "blocks" || node.Issue == nil {
			continue
		}
		blocker := model.BlockerRef{}
		if text := strings.TrimSpace(node.Issue.ID); text != "" {
			blocker.ID = &text
		}
		if text := strings.TrimSpace(node.Issue.Identifier); text != "" {
			blocker.Identifier = &text
		}
		if text := strings.TrimSpace(node.Issue.State.Name); text != "" {
			blocker.State = &text
		}
		blockers = append(blockers, blocker)
	}

	return blockers
}

func normalizeChildrenAsBlockers(connection *childIssueConnection, terminalStates []string) []model.BlockerRef {
	if connection == nil {
		return nil
	}
	terminal := make(map[string]struct{}, len(terminalStates))
	for _, state := range terminalStates {
		normalized := model.NormalizeState(state)
		if normalized != "" {
			terminal[normalized] = struct{}{}
		}
	}
	blockers := make([]model.BlockerRef, 0, len(connection.Nodes))
	for _, node := range connection.Nodes {
		normalizedState := model.NormalizeState(node.State.Name)
		if normalizedState != "" {
			if _, ok := terminal[normalizedState]; ok {
				continue
			}
		}
		blocker := model.BlockerRef{}
		if text := strings.TrimSpace(node.ID); text != "" {
			blocker.ID = &text
		}
		if text := strings.TrimSpace(node.Identifier); text != "" {
			blocker.Identifier = &text
		}
		if text := strings.TrimSpace(node.State.Name); text != "" {
			blocker.State = &text
		}
		blockers = append(blockers, blocker)
	}

	return blockers
}

func normalizePriority(value any) *int {
	switch typed := value.(type) {
	case float64:
		if typed != math.Trunc(typed) {
			return nil
		}
		priority := int(typed)
		return &priority
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil
		}
		priority := int(parsed)
		return &priority
	default:
		return nil
	}
}

func parseTime(value string) *time.Time {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return nil
	}

	return &parsed
}

func joinGraphQLErrors(errors []graphQLError) string {
	parts := make([]string, 0, len(errors))
	for _, item := range errors {
		if text := strings.TrimSpace(item.Message); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return "GraphQL errors returned without messages"
	}

	return strings.Join(parts, "; ")
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []graphQLError  `json:"errors"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type issueTeamStatesPayload struct {
	Issue *issueTeamStatesNode `json:"issue"`
}

type issueTeamStatesNode struct {
	Team *teamStatesNode `json:"team"`
}

type teamStatesNode struct {
	States *workflowStateConnection `json:"states"`
}

type workflowStateConnection struct {
	Nodes []workflowStateNode `json:"nodes"`
}

type workflowStateNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type issueUpdatePayload struct {
	IssueUpdate *issueUpdateResult `json:"issueUpdate"`
}

type issueUpdateResult struct {
	Success bool `json:"success"`
}

type issuesPayload struct {
	Issues *issueConnection `json:"issues"`
}

type issueConnection struct {
	Nodes    []issueNode `json:"nodes"`
	PageInfo pageInfo    `json:"pageInfo"`
}

type pageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type issueNode struct {
	ID               string                     `json:"id"`
	Identifier       string                     `json:"identifier"`
	Title            string                     `json:"title"`
	Description      string                     `json:"description"`
	Priority         any                        `json:"priority"`
	BranchName       string                     `json:"branchName"`
	URL              string                     `json:"url"`
	CreatedAt        string                     `json:"createdAt"`
	UpdatedAt        string                     `json:"updatedAt"`
	State            nameNode                   `json:"state"`
	Labels           *labelConnection           `json:"labels"`
	InverseRelations *inverseRelationConnection `json:"inverseRelations"`
	Children         *childIssueConnection      `json:"children"`
}

type nameNode struct {
	Name string `json:"name"`
}

type labelConnection struct {
	Nodes []labelNode `json:"nodes"`
}

type labelNode struct {
	Name string `json:"name"`
}

type inverseRelationConnection struct {
	Nodes []inverseRelationNode `json:"nodes"`
}

type inverseRelationNode struct {
	Type  string            `json:"type"`
	Issue *blockerIssueNode `json:"issue"`
}

type blockerIssueNode struct {
	ID         string   `json:"id"`
	Identifier string   `json:"identifier"`
	State      nameNode `json:"state"`
}

type childIssueConnection struct {
	Nodes []childIssueNode `json:"nodes"`
}

type childIssueNode struct {
	ID         string   `json:"id"`
	Identifier string   `json:"identifier"`
	State      nameNode `json:"state"`
}
