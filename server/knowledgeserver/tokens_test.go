package knowledgeserver

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

// One world's broken file must not hold back another world's reload: a
// token revoked in the first stays valid there until the second is fixed.
func TestTokenCoordinatorPublishesTheWorldsThatLoad(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a", "revoked"), next: tokenStore("a")}
	second := &fakeTokenRuntime{current: tokenStore("b"), loadErr: errors.New("malformed")}
	third := &fakeTokenRuntime{current: tokenStore("c")} // loads nil
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}, {name: "third", runtime: third}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}
	err = coordinator.Reload()
	if err == nil || !strings.Contains(err.Error(), `world "second": malformed`) || !strings.Contains(err.Error(), `world "third": token store is nil`) {
		t.Fatalf("Reload error = %v, want both failing worlds named", err)
	}
	if !slices.Equal(first.current.Hashes(), []string{"a"}) {
		t.Fatalf("first = %v; the revocation was held back by another world's file", first.current.Hashes())
	}
	if !slices.Equal(second.current.Hashes(), []string{"b"}) || !slices.Equal(third.current.Hashes(), []string{"c"}) {
		t.Fatal("a world whose file failed to load lost its current tokens")
	}
	if second.publishCalls != 0 || third.publishCalls != 0 {
		t.Fatalf("publish calls on failed worlds = %d, %d; want zero", second.publishCalls, third.publishCalls)
	}
}

// A world that failed to load still owns its current tokens: another world
// must not take one of them in the same reload.
func TestTokenCoordinatorChecksLoadedWorldsAgainstKeptTokens(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("b")}
	second := &fakeTokenRuntime{current: tokenStore("b"), loadErr: errors.New("malformed")}
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}, {name: "second", runtime: second}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}
	if err := coordinator.Reload(); err == nil || !strings.Contains(err.Error(), "shared by worlds") {
		t.Fatalf("Reload error = %v, want the shared token refused", err)
	}
	if first.publishCalls != 0 || !slices.Equal(first.current.Hashes(), []string{"a"}) {
		t.Fatal("a reload that failed the cross-world check published")
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

func TestTokenCoordinatorSetWorlds(t *testing.T) {
	first := &fakeTokenRuntime{current: tokenStore("a"), next: tokenStore("a")}
	coordinator, err := newTokenCoordinator([]tokenWorld{{name: "first", runtime: first}})
	if err != nil {
		t.Fatalf("newTokenCoordinator: %v", err)
	}

	second := &fakeTokenRuntime{current: tokenStore("b"), next: tokenStore("b")}
	if err := coordinator.SetWorlds([]tokenWorld{
		{name: "first", runtime: first},
		{name: "second", runtime: second},
	}); err != nil {
		t.Fatalf("SetWorlds: %v", err)
	}
	if err := coordinator.Reload(); err != nil {
		t.Fatalf("Reload after SetWorlds: %v", err)
	}

	// A replacement set violating uniqueness is rejected and the
	// previous set stays coordinated.
	dup := &fakeTokenRuntime{current: tokenStore("b"), next: tokenStore("b")}
	if err := coordinator.SetWorlds([]tokenWorld{
		{name: "second", runtime: second},
		{name: "dup", runtime: dup},
	}); err == nil || !strings.Contains(err.Error(), "shared by worlds") {
		t.Fatalf("SetWorlds err = %v, want shared-token rejection", err)
	}
	if err := coordinator.Reload(); err != nil {
		t.Fatalf("Reload after rejected SetWorlds: %v (previous set must remain valid)", err)
	}
}
