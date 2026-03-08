package tracker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"symphony-go/internal/model"
)

var errGitHubTrackerNotImplemented = errors.New("github tracker is not implemented")

type GitHubClient struct {
	httpClient     *http.Client
	configProvider func() *model.ServiceConfig
}

func NewGitHubClient(cfg *model.ServiceConfig, httpClient *http.Client) (*GitHubClient, error) {
	return NewDynamicGitHubClient(func() *model.ServiceConfig { return cfg }, httpClient)
}

func NewDynamicGitHubClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (*GitHubClient, error) {
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
	if strings.TrimSpace(cfg.TrackerOwner) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerOwner, "tracker.owner is required", nil)
	}
	if strings.TrimSpace(cfg.TrackerRepo) == "" {
		return nil, model.NewTrackerError(model.ErrMissingTrackerRepo, "tracker.repo is required", nil)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &GitHubClient{
		httpClient:     httpClient,
		configProvider: configProvider,
	}, nil
}

func (c *GitHubClient) FetchCandidateIssues(ctx context.Context) ([]model.Issue, error) {
	return nil, errGitHubTrackerNotImplemented
}

func (c *GitHubClient) FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error) {
	if len(states) == 0 {
		return []model.Issue{}, nil
	}

	return nil, errGitHubTrackerNotImplemented
}

func (c *GitHubClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error) {
	if len(ids) == 0 {
		return []model.Issue{}, nil
	}

	return nil, errGitHubTrackerNotImplemented
}
