package writepolicy_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

func defaultSeed() writepolicy.PolicySeed {
	return writepolicy.PolicySeed{
		Body:     []byte("# Write Policy\n\nDefault.\n\nstrictness: warn\n"),
		Metadata: map[string]string{"tags": "category:governance", "title": "Policy"},
	}
}

func TestSeed(t *testing.T) {
	ctx := context.Background()

	t.Run("creates a missing policy", func(t *testing.T) {
		raw := newStore(t)
		created, err := writepolicy.Seed(ctx, raw, defaultSeed())
		if err != nil || !created {
			t.Fatalf("Seed = (%v, %v), want created", created, err)
		}
		document, err := (backendtest.Direct{Store: raw}).Get(publishpolicy.DocumentPath, 0)
		if err != nil {
			t.Fatalf("get seeded policy: %v", err)
		}
		if !bytes.Equal(document.Content, defaultSeed().Body) || document.Version != 1 {
			t.Fatalf("seeded policy = version %d %q", document.Version, document.Content)
		}
	})

	t.Run("leaves a curated policy in place", func(t *testing.T) {
		raw := newStore(t)
		direct := backendtest.Direct{Store: raw}
		if _, err := direct.WriteVersion(publishpolicy.DocumentPath, 0, []byte(blockUntagged), policyMeta); err != nil {
			t.Fatalf("write curated policy: %v", err)
		}
		created, err := writepolicy.Seed(ctx, raw, defaultSeed())
		if err != nil || created {
			t.Fatalf("Seed = (%v, %v), want not created", created, err)
		}
		document, err := direct.Get(publishpolicy.DocumentPath, 0)
		if err != nil || string(document.Content) != blockUntagged || document.Version != 1 {
			t.Fatalf("policy after seed = %+v, %v, want curated v1", document, err)
		}
	})

	t.Run("an archived policy counts as present", func(t *testing.T) {
		raw := newStore(t)
		direct := backendtest.Direct{Store: raw}
		if _, err := direct.WriteVersion(publishpolicy.DocumentPath, 0, []byte(blockUntagged), policyMeta); err != nil {
			t.Fatalf("write policy: %v", err)
		}
		if _, err := direct.Archive(publishpolicy.DocumentPath, true); err != nil {
			t.Fatalf("archive policy: %v", err)
		}
		created, err := writepolicy.Seed(ctx, raw, defaultSeed())
		if err != nil || created {
			t.Fatalf("Seed over an archived policy = (%v, %v), want not created", created, err)
		}
	})

	t.Run("rejects an unenforceable seed", func(t *testing.T) {
		raw := newStore(t)
		seed := writepolicy.PolicySeed{Body: []byte("strictness: nonsense\n"), Metadata: map[string]string{"tags": "category:governance"}}
		if _, err := writepolicy.Seed(ctx, raw, seed); err == nil {
			t.Fatal("Seed accepted an unenforceable policy")
		}
		if _, err := (backendtest.Direct{Store: raw}).Get(publishpolicy.DocumentPath, 0); err == nil {
			t.Error("rejected seed was written anyway")
		}
	})

	t.Run("refuses a canceled context", func(t *testing.T) {
		raw := newStore(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := writepolicy.Seed(canceled, raw, defaultSeed()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Seed error = %v, want context canceled", err)
		}
		if _, err := (backendtest.Direct{Store: raw}).Get(publishpolicy.DocumentPath, 0); err == nil {
			t.Error("canceled seed wrote a policy anyway")
		}
	})

	t.Run("Ensure seeds, then judges what it finds", func(t *testing.T) {
		raw := newStore(t)
		if created, err := writepolicy.Ensure(ctx, raw, defaultSeed()); err != nil || !created {
			t.Fatalf("Ensure on an empty world = (%v, %v), want created", created, err)
		}
		if created, err := writepolicy.Ensure(ctx, raw, defaultSeed()); err != nil || created {
			t.Fatalf("Ensure on a seeded world = (%v, %v), want valid and not created", created, err)
		}
		if _, err := (backendtest.Direct{Store: raw}).Archive(publishpolicy.DocumentPath, true); err != nil {
			t.Fatalf("archive policy: %v", err)
		}
		if _, err := writepolicy.Ensure(ctx, raw, defaultSeed()); !errors.Is(err, writepolicy.ErrInvalidPolicy) {
			t.Fatalf("Ensure over an archived policy = %v, want ErrInvalidPolicy", err)
		}
	})
}
