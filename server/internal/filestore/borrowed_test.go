package filestore

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

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

// Close waits for a read it already admitted and only then unlocks, so no
// read runs after the store lock is gone.
func TestCloseWaitsForAdmittedRead(t *testing.T) {
	ctx := context.Background()
	s := New(store.New(t.TempDir()), catalog.New())
	opened, err := s.OpenReadView(ctx)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	view := opened.(*readView)

	admitted, release := make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		_, err := admit(view, func(reader) (struct{}, error) {
			close(admitted)
			<-release
			return struct{}{}, nil
		})
		readDone <- err
	}()
	<-admitted

	closed := make(chan error, 1)
	go func() { closed <- view.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while an admitted read was still running", err)
	case <-time.After(50 * time.Millisecond):
	}
	// The read lock is still held, so a writer cannot have got in either.
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("store write lock was free while an admitted read was running")
	}

	close(release)
	if err := <-readDone; err != nil {
		t.Errorf("admitted read: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := view.Get(ctx, "/a.md", 0); !errors.Is(err, backend.ErrViewClosed) {
		t.Errorf("read after close: %v, want ErrViewClosed", err)
	}
	if !s.mu.TryLock() {
		t.Fatal("store lock still held after Close")
	}
	s.mu.Unlock()
}
