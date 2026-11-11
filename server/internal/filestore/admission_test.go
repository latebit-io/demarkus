package filestore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	protocolstore "github.com/latebit-io/demarkus/server/store"
)

// admissionWait bounds how long a canceled waiter may take to return; far
// above scheduling noise, far below "waited for the holder".
const admissionWait = 2 * time.Second

// waitWriterQueued returns once a writer waits behind a held view: FIFO, a
// queued writer makes even a one-unit TryAcquire fail.
func waitWriterQueued(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(admissionWait)
	for s.mu.sem.TryAcquire(1) {
		s.mu.sem.Release(1)
		if time.Now().After(deadline) {
			t.Fatal("the writer never queued behind the view")
		}
		time.Sleep(time.Millisecond)
	}
}

// requireCanceled reports whether the canceled waiter returned in time, with
// context.Canceled.
func requireCanceled(t *testing.T, result <-chan error, what string) bool {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v, want canceled", what, err)
		}
		return true
	case <-time.After(admissionWait):
		t.Errorf("canceled %s still waiting", what)
		return false
	}
}

func newAdmissionStore(t *testing.T) *Store {
	t.Helper()
	s := New(protocolstore.New(t.TempDir()), catalog.New())
	if _, err := s.Publish(t.Context(), backend.WriteRequest{Path: "/a.md", Content: []byte("# v1\n")}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

// A write queued behind an open read view returns when its context is
// canceled, and never mutates once the view closes.
func TestCanceledWriteBehindReadViewNeverLands(t *testing.T) {
	writes := map[string]func(*Store, context.Context) error{
		"publish": func(s *Store, ctx context.Context) error {
			_, err := s.Publish(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: 1, Content: []byte("# v2\n")})
			return err
		},
		"append": func(s *Store, ctx context.Context) error {
			_, err := s.Append(ctx, backend.WriteRequest{Path: "/a.md", Content: []byte("more\n")})
			return err
		},
		"archive": func(s *Store, ctx context.Context) error {
			_, err := s.SetArchived(ctx, backend.ArchiveRequest{Path: "/a.md", Archived: true})
			return err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			s := newAdmissionStore(t)
			view, err := s.OpenReadView(t.Context())
			if err != nil {
				t.Fatalf("open view: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- write(s, ctx) }()
			waitWriterQueued(t, s)
			cancel()
			returned := requireCanceled(t, done, "queued "+name)
			if err := view.Close(); err != nil {
				t.Fatalf("close view: %v", err)
			}
			if !returned { // a waiter that outlived its cancel: the version check judges it
				select {
				case err := <-done:
					t.Logf("queued %s returned after the view closed: %v", name, err)
				case <-time.After(admissionWait):
				}
			}
			doc, err := s.documents.Get("/a.md", 0)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if doc.Version != 1 || doc.Archived {
				t.Errorf("after the view closed: version %d archived %v, want version 1 live", doc.Version, doc.Archived)
			}
		})
	}
}

// OpenReadView queued behind a write returns when canceled, and leaks no
// lock: once the write ends, views and writes are admitted again.
func TestCanceledReadViewBehindWriteLeaksNothing(t *testing.T) {
	s := newAdmissionStore(t)
	inCommit, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		_, err := s.Publish(t.Context(), backend.WriteRequest{
			Path: "/a.md", ExpectedVersion: 1, Content: []byte("# v2\n"),
			Precondition: func(context.Context, backend.CommitState, storefmt.PreparedWrite) error {
				close(inCommit)
				<-release
				return nil
			},
		})
		held <- err
	}()
	<-inCommit

	ctx, cancel := context.WithCancel(t.Context())
	opened := make(chan error, 1)
	go func() {
		view, err := s.OpenReadView(ctx)
		if err == nil {
			err = errors.Join(errors.New("view opened"), view.Close())
		}
		opened <- err
	}()
	// A queued view is invisible to TryAcquire, so it gets time to block. On a
	// slow runner the cancel can land first: a false pass, never a false failure.
	time.Sleep(50 * time.Millisecond)
	cancel()
	requireCanceled(t, opened, "queued OpenReadView")

	close(release)
	if err := <-held; err != nil {
		t.Fatalf("held write: %v", err)
	}
	view, err := s.OpenReadView(t.Context())
	if err != nil {
		t.Fatalf("view after the write: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close view: %v", err)
	}
	if _, err := s.Publish(t.Context(), backend.WriteRequest{Path: "/a.md", ExpectedVersion: 2, Content: []byte("# v3\n")}); err != nil {
		t.Errorf("write after the canceled view: %v", err)
	}
}

// Close waits for open views, then refuses writes; a canceled waiter does not
// hold it up.
func TestCloseAfterCanceledWaiter(t *testing.T) {
	s := newAdmissionStore(t)
	view, err := s.OpenReadView(t.Context())
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan error, 1)
	go func() {
		_, err := s.Publish(ctx, backend.WriteRequest{Path: "/a.md", ExpectedVersion: 1, Content: []byte("# v2\n")})
		queued <- err
	}()
	waitWriterQueued(t, s)
	cancel()
	requireCanceled(t, queued, "queued write")

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	if err := view.Close(); err != nil {
		t.Fatalf("close view: %v", err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(admissionWait):
		t.Fatal("Close still waiting after the view closed")
	}
	if _, err := s.Publish(t.Context(), backend.WriteRequest{Path: "/a.md", ExpectedVersion: 1, Content: []byte("# v2\n")}); !errors.Is(err, ErrClosed) {
		t.Errorf("write after Close = %v, want ErrClosed", err)
	}
}
