package secret

import (
	"fmt"
	"os"
	"sort"
	"sync"
)

// Resolver 是统一的环境变量查找函数签名。
type Resolver func(key string) (string, bool)

// DefaultResolver 是全局默认解析器；默认值保持为 os.LookupEnv。
var DefaultResolver Resolver = os.LookupEnv

type managedEntry struct {
	value       string
	hadOriginal bool
	original    string
}

// Store 是进程级 secret 管理器。
type Store struct {
	mu      sync.RWMutex
	managed map[string]managedEntry
}

func New() *Store {
	return &Store{managed: map[string]managedEntry{}}
}

func (s *Store) Get(key string) (string, bool) {
	if s == nil {
		return os.LookupEnv(key)
	}

	s.mu.RLock()
	entry, ok := s.managed[key]
	s.mu.RUnlock()
	if ok {
		return entry.value, true
	}

	return os.LookupEnv(key)
}

func (s *Store) Set(key string, value string) error {
	if s == nil {
		return fmt.Errorf("secret store is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, managed := s.managed[key]
	if !managed {
		entry.original, entry.hadOriginal = os.LookupEnv(key)
	}
	if err := os.Setenv(key, value); err != nil {
		return err
	}

	entry.value = value
	s.managed[key] = entry
	return nil
}

func (s *Store) Delete(key string) error {
	if s == nil {
		return fmt.Errorf("secret store is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.managed[key]
	if !ok {
		return fmt.Errorf("secret %s is not managed", key)
	}

	var err error
	if entry.hadOriginal {
		err = os.Setenv(key, entry.original)
	} else {
		err = os.Unsetenv(key)
	}
	if err != nil {
		return err
	}

	delete(s.managed, key)
	return nil
}

func (s *Store) ManagedKeys() []string {
	if s == nil {
		return nil
	}

	s.mu.RLock()
	keys := make([]string, 0, len(s.managed))
	for key := range s.managed {
		keys = append(keys, key)
	}
	s.mu.RUnlock()

	sort.Strings(keys)
	return keys
}

func (s *Store) Resolver() Resolver {
	return os.LookupEnv
}
