package tracker

import (
	"context"
	"fmt"
	"net/http"

	"symphony-go/internal/model"
)

type Client interface {
	FetchCandidateIssues(ctx context.Context) ([]model.Issue, error)
	FetchIssuesByStates(ctx context.Context, states []string) ([]model.Issue, error)
	FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]model.Issue, error)
}

func NewClient(cfg *model.ServiceConfig, httpClient *http.Client) (Client, error) {
	if cfg == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config is nil", nil)
	}

	switch model.NormalizeState(cfg.TrackerKind) {
	case "linear":
		return NewLinearClient(cfg, httpClient)
	case "github":
		return NewGitHubClient(cfg, httpClient)
	default:
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.TrackerKind), nil)
	}
}

func NewDynamicClient(configProvider func() *model.ServiceConfig, httpClient *http.Client) (Client, error) {
	if configProvider == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config provider is nil", nil)
	}
	cfg := configProvider()
	if cfg == nil {
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, "service config is nil", nil)
	}

	switch model.NormalizeState(cfg.TrackerKind) {
	case "linear":
		return NewDynamicLinearClient(configProvider, httpClient)
	case "github":
		return NewDynamicGitHubClient(configProvider, httpClient)
	default:
		return nil, model.NewTrackerError(model.ErrUnsupportedTrackerKind, fmt.Sprintf("unsupported tracker.kind %q", cfg.TrackerKind), nil)
	}
}
