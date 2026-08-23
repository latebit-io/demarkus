package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/auth"
)

func TestTokenCoordinator(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("c")}
	second := &fakeTokenRuntime{current: tokenStore("b"), next: tokenStore("d")}
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}
	if err := coordinator.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !slices.Equal(first.current.Hashes(), []string{"c"}) || !slices.Equal(second.current.Hashes(), []string{"d"}) {
		t.Fatalf("published hashes = %v, %v", first.current.Hashes(), second.current.Hashes())
	}
}

func TestTokenCoordinatorRejectsDuplicates(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		_, err := newTokenCoordinator([]tokenWorld{
			{name: "first", runtime: &fakeTokenRuntime{current: tokenStore("shared")}},
			{name: "second", runtime: &fakeTokenRuntime{current: tokenStore("shared")}},
		})
		if err == nil || !strings.Contains(err.Error(), "shared by worlds") {
			t.Fatalf("error = %v, want shared token error", err)
		}
	})

	t.Run("reload remains atomic", func(t *testing.T) {
		first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("shared")}
		second := &fakeTokenRuntime{current: tokenStore("b"), next: tokenStore("shared")}
		coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
		if err != nil {
			t.Fatalf("newTokenCoordinator: %v", err)
		}
		if err := coordinator.Reload(); err == nil {
			t.Fatal("Reload accepted duplicate token")
		}
		if !slices.Equal(first.current.Hashes(), []string{"a"}) || !slices.Equal(second.current.Hashes(), []string{"b"}) {
			t.Fatal("failed reload partially published candidates")
		}
	})

	t.Run("live transfer", func(t *testing.T) {
		first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("c")}
		second := &fakeTokenRuntime{current: tokenStore("b"), next: tokenStore("a")}
		coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
		if err != nil {
			t.Fatalf("newTokenCoordinator: %v", err)
		}
		if err := coordinator.Reload(); err == nil || !strings.Contains(err.Error(), `from world "first" to world "second"`) {
			t.Fatalf("Reload error = %v, want live transfer error", err)
		}
	})
}

func TestTokenCoordinatorLoadFailureKeepsCurrent(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("c")}
	second := &fakeTokenRuntime{current: tokenStore("b"), loadErr: errors.New("malformed")}
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}
	if err := coordinator.Reload(); err == nil {
		t.Fatal("Reload accepted load failure")
	}
	if !slices.Equal(first.current.Hashes(), []string{"a"}) || !slices.Equal(second.current.Hashes(), []string{"b"}) {
		t.Fatal("load failure partially published candidates")
	}
}

func TestTokenCoordinatorNilLoadKeepsCurrent(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("c")}
	second := &fakeTokenRuntime{current: tokenStore("b")}
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}
	if err := coordinator.Reload(); err == nil || !strings.Contains(err.Error(), `world "second": token store is nil`) {
		t.Fatalf("Reload error = %v, want nil-store error", err)
	}
	if first.publishCalls != 0 || second.publishCalls != 0 {
		t.Fatalf("publish calls = %d, %d; want zero", first.publishCalls, second.publishCalls)
	}
	if !slices.Equal(first.current.Hashes(), []string{"a"}) || !slices.Equal(second.current.Hashes(), []string{"b"}) {
		t.Fatal("nil load partially published candidates")
	}
}

type fakeTokenRuntime struct {
	current      *auth.TokenStore
	next         *auth.TokenStore
	loadErr      error
	publishCalls int
}

func (runtime *fakeTokenRuntime) Tokens() *auth.TokenStore { return runtime.current }
func (runtime *fakeTokenRuntime) LoadTokens() (*auth.TokenStore, error) {
	return runtime.next, runtime.loadErr
}
func (runtime *fakeTokenRuntime) PublishTokens(store *auth.TokenStore) error {
	runtime.publishCalls++
	runtime.current = store
	return nil
}

func tokenStore(hashes ...string) *auth.TokenStore {
	tokens := make(map[string]auth.Token, len(hashes))
	for _, hash := range hashes {
		tokens[hash] = auth.Token{}
	}
	return auth.NewTokenStore(tokens)
}
