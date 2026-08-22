package storetest

import (
	"fmt"
	"os"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/handler"
)

type currentVersionResultStore struct {
	handler.DocumentStore
	version int
	err     error
}

func (s *currentVersionResultStore) CurrentVersionResult(string) (int, error) {
	return s.version, s.err
}

func TestCurrentBoth(t *testing.T) {
	t.Run("matching error classes continue with zero", func(t *testing.T) {
		ref := LookupBackend{Store: &currentVersionResultStore{version: 4, err: os.ErrNotExist}}
		cand := LookupBackend{Store: &currentVersionResultStore{version: 9, err: fmt.Errorf("wrapped: %w", os.ErrNotExist)}}

		version, ok := newDiffRun(t, 1).currentBoth(ref, cand, "/../invalid.md")
		if version != 0 || !ok {
			t.Errorf("currentBoth = (%d, %v), want (0, true)", version, ok)
		}
	})

	t.Run("successful versions compare normally", func(t *testing.T) {
		ref := LookupBackend{Store: &currentVersionResultStore{version: 3}}
		cand := LookupBackend{Store: &currentVersionResultStore{version: 3}}

		version, ok := newDiffRun(t, 1).currentBoth(ref, cand, "/doc.md")
		if version != 3 || !ok {
			t.Errorf("currentBoth = (%d, %v), want (3, true)", version, ok)
		}
	})
}
