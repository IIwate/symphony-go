package secret

import (
	"fmt"
	"strings"
	"sync"
)

type ReferenceKind string

const (
	ReferenceKindEnv      ReferenceKind = "env"
	ReferenceKindProvider ReferenceKind = "provider"
)

func (k ReferenceKind) IsValid() bool {
	switch k {
	case ReferenceKindEnv, ReferenceKindProvider:
		return true
	default:
		return false
	}
}

type ProviderReference struct {
	Name     string
	SecretID string
	Version  *string
}

type Reference struct {
	Kind     ReferenceKind
	Name     string
	Provider *ProviderReference
}

func (r Reference) Validate() error {
	if !r.Kind.IsValid() {
		return fmt.Errorf("invalid reference kind %q", r.Kind)
	}
	switch r.Kind {
	case ReferenceKindEnv:
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("env reference name is required")
		}
	case ReferenceKindProvider:
		if r.Provider == nil {
			return fmt.Errorf("provider reference payload is required")
		}
		if strings.TrimSpace(r.Provider.Name) == "" {
			return fmt.Errorf("provider reference name is required")
		}
		if strings.TrimSpace(r.Provider.SecretID) == "" {
			return fmt.Errorf("provider reference secret_id is required")
		}
	}
	return nil
}

type Provider interface {
	Resolve(secretID string, version *string) (string, bool, error)
}

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

func (r *Registry) Register(name string, provider Provider) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = provider
}

func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[name]
	return ok
}

func (r *Registry) Resolve(ref Reference, env Resolver) (string, bool, error) {
	if err := ref.Validate(); err != nil {
		return "", false, err
	}
	switch ref.Kind {
	case ReferenceKindEnv:
		if env == nil {
			env = DefaultResolver
		}
		value, ok := env(ref.Name)
		return value, ok, nil
	case ReferenceKindProvider:
		if r == nil {
			return "", false, fmt.Errorf("secret registry is nil")
		}
		r.mu.RLock()
		provider, ok := r.providers[ref.Provider.Name]
		r.mu.RUnlock()
		if !ok {
			return "", false, fmt.Errorf("secret provider %q is not registered", ref.Provider.Name)
		}
		return provider.Resolve(ref.Provider.SecretID, ref.Provider.Version)
	default:
		return "", false, fmt.Errorf("unsupported secret reference kind %q", ref.Kind)
	}
}

var DefaultRegistry = NewRegistry()
