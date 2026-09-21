package storetest

import (
	"context"
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// versionsStore answers only Versions, which is all CurrentVersion reads.
type versionsStore struct {
	handler.DocumentStore
	version int
	err     error
}

func (s *versionsStore) OpenReadView(context.Context) (backend.ReadView, error) {
	return &versionsView{store: s}, nil
}

type versionsView struct {
	backend.ReadView
	store *versionsStore
}

func (v *versionsView) Versions(context.Context, string) ([]storefmt.VersionInfo, error) {
	if v.store.err != nil {
		return nil, v.store.err
	}
	return []storefmt.VersionInfo{{Version: v.store.version}}, nil
}

func (*versionsView) Close() error { return nil }

func TestCurrentBoth(t *testing.T) {
	t.Run("matching error classes continue with zero", func(t *testing.T) {
		ref := LookupBackend{Store: &versionsStore{version: 4, err: storefmt.ErrIntegrity}}
		cand := LookupBackend{Store: &versionsStore{version: 9, err: fmt.Errorf("wrapped: %w", storefmt.ErrIntegrity)}}

		version, ok := newDiffRun(t, 1).currentBoth(ref, cand, "/broken.md")
		if version != 0 || !ok {
			t.Errorf("currentBoth = (%d, %v), want (0, true)", version, ok)
		}
	})

	t.Run("a missing document is version zero on both", func(t *testing.T) {
		ref := LookupBackend{Store: &versionsStore{err: backend.ErrNotFound}}
		cand := LookupBackend{Store: &versionsStore{err: fmt.Errorf("wrapped: %w", backend.ErrNotFound)}}

		version, ok := newDiffRun(t, 1).currentBoth(ref, cand, "/missing.md")
		if version != 0 || !ok {
			t.Errorf("currentBoth = (%d, %v), want (0, true)", version, ok)
		}
	})

	t.Run("successful versions compare normally", func(t *testing.T) {
		ref := LookupBackend{Store: &versionsStore{version: 3}}
		cand := LookupBackend{Store: &versionsStore{version: 3}}

		version, ok := newDiffRun(t, 1).currentBoth(ref, cand, "/doc.md")
		if version != 3 || !ok {
			t.Errorf("currentBoth = (%d, %v), want (3, true)", version, ok)
		}
	})
}
