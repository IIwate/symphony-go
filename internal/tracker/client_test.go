package tracker

import (
	"errors"
	"testing"

	"symphony-go/internal/model"
)

func TestNewClientRoutesByTrackerKind(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *model.ServiceConfig
		wantType  string
		wantError error
	}{
		{
			name: "linear client",
			cfg: &model.ServiceConfig{
				TrackerKind:        "linear",
				TrackerAPIKey:      "secret",
				TrackerProjectSlug: "demo",
			},
			wantType: "linear",
		},
		{
			name: "github client",
			cfg: &model.ServiceConfig{
				TrackerKind:   "GitHub",
				TrackerAPIKey: "secret",
				TrackerOwner:  "octocat",
				TrackerRepo:   "demo",
			},
			wantType: "github",
		},
		{
			name: "unsupported tracker kind",
			cfg: &model.ServiceConfig{
				TrackerKind: "jira",
			},
			wantError: model.ErrUnsupportedTrackerKind,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, err := NewClient(tc.cfg, nil)
			if tc.wantError != nil {
				if !errors.Is(err, tc.wantError) {
					t.Fatalf("NewClient() error = %v, want %v", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}

			switch tc.wantType {
			case "linear":
				if _, ok := client.(*LinearClient); !ok {
					t.Fatalf("NewClient() returned %T, want *LinearClient", client)
				}
			case "github":
				if _, ok := client.(*GitHubClient); !ok {
					t.Fatalf("NewClient() returned %T, want *GitHubClient", client)
				}
			}
		})
	}
}

func TestNewDynamicClientRoutesByTrackerKind(t *testing.T) {
	provider := func(cfg *model.ServiceConfig) func() *model.ServiceConfig {
		return func() *model.ServiceConfig {
			return cfg
		}
	}

	linearClient, err := NewDynamicClient(provider(&model.ServiceConfig{
		TrackerKind:        "linear",
		TrackerAPIKey:      "secret",
		TrackerProjectSlug: "demo",
	}), nil)
	if err != nil {
		t.Fatalf("NewDynamicClient(linear) error = %v", err)
	}
	if _, ok := linearClient.(*LinearClient); !ok {
		t.Fatalf("NewDynamicClient(linear) returned %T, want *LinearClient", linearClient)
	}

	githubClient, err := NewDynamicClient(provider(&model.ServiceConfig{
		TrackerKind:   "github",
		TrackerAPIKey: "secret",
		TrackerOwner:  "octocat",
		TrackerRepo:   "demo",
	}), nil)
	if err != nil {
		t.Fatalf("NewDynamicClient(github) error = %v", err)
	}
	if _, ok := githubClient.(*GitHubClient); !ok {
		t.Fatalf("NewDynamicClient(github) returned %T, want *GitHubClient", githubClient)
	}
}

func TestNewGitHubClientRequiresOwnerAndRepo(t *testing.T) {
	base := &model.ServiceConfig{
		TrackerKind:   "github",
		TrackerAPIKey: "secret",
		TrackerOwner:  "octocat",
		TrackerRepo:   "demo",
	}

	tests := []struct {
		name      string
		mutate    func(*model.ServiceConfig)
		wantError error
	}{
		{
			name: "missing api key",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerAPIKey = ""
			},
			wantError: model.ErrMissingTrackerAPIKey,
		},
		{
			name: "missing owner",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerOwner = ""
			},
			wantError: model.ErrMissingTrackerOwner,
		},
		{
			name: "missing repo",
			mutate: func(cfg *model.ServiceConfig) {
				cfg.TrackerRepo = ""
			},
			wantError: model.ErrMissingTrackerRepo,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *base
			tc.mutate(&cfg)

			_, err := NewGitHubClient(&cfg, nil)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("NewGitHubClient() error = %v, want %v", err, tc.wantError)
			}
		})
	}
}
