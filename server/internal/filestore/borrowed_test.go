package filestore

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// A precondition is lent a reader under the write lock. Closing it must not
// release a read lock nobody took, which would be a fatal unlock.
func TestPreconditionReaderCloseIsHarmless(t *testing.T) {
	ctx := context.Background()
	s := New(store.New(t.TempDir()), catalog.New())
	closeLent := func(state backend.Reader) error {
		if closer, ok := state.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				return err
			}
		}
		_, err := state.IsDir(ctx, "/")
		return err
	}
	write := backend.WriteRequest{
		Path: "/a.md", Content: []byte("# A\n"),
		Precondition: func(_ context.Context, state backend.Reader, _ storefmt.PreparedWrite) error { return closeLent(state) },
	}
	if _, err := s.Publish(ctx, write); err != nil {
		t.Fatalf("publish: %v", err)
	}
	archive := backend.ArchiveRequest{
		Path: "/a.md", Archived: true,
		Precondition: func(_ context.Context, state backend.Reader, _ storefmt.ArchiveChange) error { return closeLent(state) },
	}
	if _, err := s.SetArchived(ctx, archive); err != nil {
		t.Fatalf("archive: %v", err)
	}
	// The store still takes locks normally afterwards.
	view, err := s.OpenReadView(ctx)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close view: %v", err)
	}
}

// Close waits for admitted reads, and a writer blocked behind the view gets the
// lock only after them: every read is whole or refused, never after the unlock.
func TestCloseRacesReadsSafely(t *testing.T) {
	ctx := context.Background()
	s := New(store.New(t.TempDir()), catalog.New())
	if _, err := s.Publish(ctx, backend.WriteRequest{Path: "/a.md", Content: []byte("# A\n")}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	view, err := s.OpenReadView(ctx)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 50 {
				if _, err := view.Get(ctx, "/a.md", 0); err != nil && !errors.Is(err, backend.ErrViewClosed) {
					t.Errorf("read during close: %v", err)
				}
			}
		})
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := s.Publish(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: 1, Content: []byte("# B\n")}); err != nil {
		t.Fatalf("publish after close: %v", err)
	}
	readers.Wait()
}
