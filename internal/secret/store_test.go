package secret

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestStoreSetAndDeleteRestoresOriginalValue(t *testing.T) {
	key := "SYMPHONY_SECRET_STORE_RESTORE"
	t.Setenv(key, "original")

	store := New()
	if err := store.Set(key, "updated"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, ok := store.Get(key); !ok || got != "updated" {
		t.Fatalf("Get() = (%q, %t), want (updated, true)", got, ok)
	}
	if got := os.Getenv(key); got != "updated" {
		t.Fatalf("os.Getenv() = %q, want updated", got)
	}

	if err := store.Delete(key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := os.Getenv(key); got != "original" {
		t.Fatalf("os.Getenv() after Delete = %q, want original", got)
	}
}

func TestStoreDeleteUnmanagedKeyFails(t *testing.T) {
	store := New()
	if err := store.Delete("SYMPHONY_SECRET_STORE_UNKNOWN"); err == nil {
		t.Fatal("Delete() error = nil, want failure for unmanaged key")
	}
}

func TestStoreManagedKeysAreSorted(t *testing.T) {
	store := New()
	for _, item := range []struct {
		key   string
		value string
	}{
		{key: "SYMPHONY_SECRET_B", value: "b"},
		{key: "SYMPHONY_SECRET_A", value: "a"},
	} {
		if err := store.Set(item.key, item.value); err != nil {
			t.Fatalf("Set(%q) error = %v", item.key, err)
		}
		t.Cleanup(func() { _ = store.Delete(item.key) })
	}

	keys := store.ManagedKeys()
	if strings.Join(keys, ",") != "SYMPHONY_SECRET_A,SYMPHONY_SECRET_B" {
		t.Fatalf("ManagedKeys() = %v, want sorted keys", keys)
	}
}

func TestStoreResolverReadsProcessEnv(t *testing.T) {
	key := "SYMPHONY_SECRET_STORE_RESOLVER"
	store := New()
	if err := store.Set(key, "resolver-value"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(key) })

	value, ok := store.Resolver()(key)
	if !ok || value != "resolver-value" {
		t.Fatalf("Resolver() = (%q, %t), want (resolver-value, true)", value, ok)
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	key := "SYMPHONY_SECRET_STORE_CONCURRENT"
	store := New()
	t.Cleanup(func() { _ = store.Delete(key) })

	var wg sync.WaitGroup
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if err := store.Set(key, "value"); err != nil {
				t.Errorf("Set() error = %v", err)
				return
			}
			if _, ok := store.Get(key); !ok {
				t.Error("Get() ok = false, want true")
			}
			_ = store.ManagedKeys()
		}(index)
	}
	wg.Wait()

	if err := store.Delete(key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}
