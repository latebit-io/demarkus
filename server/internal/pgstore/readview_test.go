package pgstore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
	"github.com/latebit-io/demarkus/server/internal/pgstore/pgtest"
)

func TestPostgresReadViewContract(t *testing.T) {
	t.Run("options context and rollback error", func(t *testing.T) {
		rollbackErr := errors.New("rollback failed")
		state := &readViewDriverState{rollbackErr: rollbackErr}
		db := newReadViewDriverDB(t, state)
		view, err := pgstore.NewWithDB(db, nil).OpenReadView()
		if err != nil {
			t.Fatalf("open read view: %v", err)
		}

		snapshot := state.snapshot()
		if snapshot.options.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
			t.Errorf("isolation = %d, want repeatable read", snapshot.options.Isolation)
		}
		if !snapshot.options.ReadOnly {
			t.Error("transaction is not read-only")
		}
		deadline, ok := snapshot.beginCtx.Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining <= 0 || remaining > 10*time.Second {
			t.Errorf("view context deadline = %v, want active 10-second deadline", deadline)
		}

		if err := view.Close(); !errors.Is(err, rollbackErr) {
			t.Errorf("close error = %v, want %v", err, rollbackErr)
		}
		if err := view.Close(); !errors.Is(err, rollbackErr) {
			t.Errorf("second close error = %v, want %v", err, rollbackErr)
		}
		snapshot = state.snapshot()
		if snapshot.rollbacks != 1 {
			t.Errorf("rollbacks = %d, want 1", snapshot.rollbacks)
		}
		if !errors.Is(snapshot.beginCtx.Err(), context.Canceled) {
			t.Errorf("view context error = %v, want canceled", snapshot.beginCtx.Err())
		}
	})

	t.Run("transaction done is ignored", func(t *testing.T) {
		state := &readViewDriverState{rollbackErr: sql.ErrTxDone}
		view, err := pgstore.NewWithDB(newReadViewDriverDB(t, state), nil).OpenReadView()
		if err != nil {
			t.Fatalf("open read view: %v", err)
		}
		if err := view.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if err := view.Close(); err != nil {
			t.Errorf("second close: %v", err)
		}
		if got := state.snapshot().rollbacks; got != 1 {
			t.Errorf("rollbacks = %d, want 1", got)
		}
	})

	t.Run("begin failure cancels context", func(t *testing.T) {
		beginErr := errors.New("begin failed")
		state := &readViewDriverState{beginErr: beginErr}
		view, err := pgstore.NewWithDB(newReadViewDriverDB(t, state), nil).OpenReadView()
		if view != nil {
			t.Error("view is non-nil after begin failure")
		}
		if !errors.Is(err, beginErr) {
			t.Errorf("open error = %v, want %v", err, beginErr)
		}
		if got := state.snapshot().beginCtx.Err(); !errors.Is(got, context.Canceled) {
			t.Errorf("view context error = %v, want canceled", got)
		}
	})

	t.Run("hash query failure remains an error", func(t *testing.T) {
		queryErr := errors.New("query failed")
		state := &readViewDriverState{queryErr: queryErr}
		view, err := pgstore.NewWithDB(newReadViewDriverDB(t, state), nil).OpenReadView()
		if err != nil {
			t.Fatalf("open read view: %v", err)
		}
		t.Cleanup(func() {
			if err := view.Close(); err != nil {
				t.Errorf("close read view: %v", err)
			}
		})

		_, err = view.LookupHash("sha256-deadbeef")
		if !errors.Is(err, queryErr) {
			t.Errorf("lookup hash error = %v, want %v", err, queryErr)
		}
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("lookup hash error = %v, must not report not-found", err)
		}
		snapshot := state.snapshot()
		if snapshot.opens != 1 {
			t.Errorf("connections opened = %d, want 1", snapshot.opens)
		}
		if snapshot.queryCtx != snapshot.beginCtx {
			t.Error("query did not use the view context")
		}
	})
}

func TestPostgresReadViewSnapshot(t *testing.T) {
	t.Run("pins document hash list versions lookup and chain", func(t *testing.T) {
		s := openStore(t)
		if err := s.Reset(context.Background()); err != nil {
			t.Fatalf("reset: %v", err)
		}
		firstBody := []byte("# First\n")
		beforeBody := []byte("# Before\n")
		afterBody := []byte("# After\n")
		if _, err := s.WriteVersion("/docs/doc.md", 0, firstBody, map[string]string{"tags": "before"}); err != nil {
			t.Fatalf("write v1: %v", err)
		}
		if _, err := s.WriteVersion("/docs/doc.md", 1, beforeBody, map[string]string{"tags": "before"}); err != nil {
			t.Fatalf("write v2: %v", err)
		}

		view, err := s.OpenReadView()
		if err != nil {
			t.Fatalf("open original view: %v", err)
		}
		t.Cleanup(func() {
			if err := view.Close(); err != nil {
				t.Errorf("close original view: %v", err)
			}
		})
		doc, err := view.Get("/docs/doc.md", 0)
		if err != nil {
			t.Fatalf("establish original snapshot: %v", err)
		}
		if doc.Version != 2 {
			t.Fatalf("original snapshot version = %d, want 2", doc.Version)
		}

		if _, err := s.WriteVersion("/docs/doc.md", 2, afterBody, map[string]string{"tags": "after"}); err != nil {
			t.Fatalf("write v3: %v", err)
		}
		if _, err := s.WriteVersion("/docs/new.md", 0, []byte("# New\n"), map[string]string{"tags": "new"}); err != nil {
			t.Fatalf("write new document: %v", err)
		}
		pgtest.TamperVersion(t, pgSchema, "/docs/doc.md", 1, []byte("tampered"))

		assertPostgresReadView(t, view, &readViewWant{
			body:          beforeBody,
			version:       2,
			hash:          protocolstore.ContentHash(beforeBody),
			missingHash:   protocolstore.ContentHash(afterBody),
			entries:       []protocolstore.DirEntry{{Name: "doc.md"}},
			versions:      []int{2, 1},
			lookupQuery:   "before",
			missingLookup: "after",
			chainValid:    true,
		})
		if err := view.Close(); err != nil {
			t.Fatalf("close original view: %v", err)
		}

		fresh, err := s.OpenReadView()
		if err != nil {
			t.Fatalf("open fresh view: %v", err)
		}
		t.Cleanup(func() {
			if err := fresh.Close(); err != nil {
				t.Errorf("close fresh view: %v", err)
			}
		})
		assertPostgresReadView(t, fresh, &readViewWant{
			body:          afterBody,
			version:       3,
			hash:          protocolstore.ContentHash(afterBody),
			missingHash:   protocolstore.ContentHash(beforeBody),
			entries:       []protocolstore.DirEntry{{Name: "doc.md"}, {Name: "new.md"}},
			versions:      []int{3, 2, 1},
			lookupQuery:   "after",
			missingLookup: "before",
			chainValid:    false,
		})
	})

	t.Run("pins authoritative archive state", func(t *testing.T) {
		s := openStore(t)
		if _, err := s.WriteVersion("/archive.md", 0, []byte("body"), nil); err != nil {
			t.Fatalf("write: %v", err)
		}
		view, err := s.OpenReadView()
		if err != nil {
			t.Fatalf("open original view: %v", err)
		}
		t.Cleanup(func() {
			if err := view.Close(); err != nil {
				t.Errorf("close original archive view: %v", err)
			}
		})
		if doc, err := view.Get("/archive.md", 0); err != nil || doc.Archived {
			t.Fatalf("establish original snapshot = (%+v, %v), want live", doc, err)
		}
		if _, _, err := s.Archive("/archive.md", true); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if doc, err := view.Get("/archive.md", 0); err != nil || doc.Archived {
			t.Errorf("original snapshot after archive = (%+v, %v), want live", doc, err)
		}
		if err := view.Close(); err != nil {
			t.Fatalf("close original view: %v", err)
		}

		fresh, err := s.OpenReadView()
		if err != nil {
			t.Fatalf("open fresh view: %v", err)
		}
		defer func() {
			if err := fresh.Close(); err != nil {
				t.Errorf("close fresh view: %v", err)
			}
		}()
		if doc, err := fresh.Get("/archive.md", 0); err != nil || !doc.Archived {
			t.Errorf("fresh snapshot = (%+v, %v), want archived", doc, err)
		}
		if doc, err := fresh.Get("/archive.md", 1); err != nil || doc.Archived {
			t.Errorf("fresh pinned read = (%+v, %v), want Archived false", doc, err)
		}
	})
}

type readViewWant struct {
	body          []byte
	version       int
	hash          string
	missingHash   string
	entries       []protocolstore.DirEntry
	versions      []int
	lookupQuery   string
	missingLookup string
	chainValid    bool
}

func assertPostgresReadView(t *testing.T, view backend.ReadView, want *readViewWant) {
	t.Helper()
	doc, err := view.Get("/docs/doc.md", 0)
	if err != nil {
		t.Errorf("get: %v", err)
	} else if doc.Version != want.version || !reflect.DeepEqual(doc.Content, want.body) {
		t.Errorf("get = v%d %q, want v%d %q", doc.Version, doc.Content, want.version, want.body)
	}
	if isDir, err := view.IsDir("/docs"); err != nil || !isDir {
		t.Errorf("isdir = %v, error %v; want true", isDir, err)
	}
	entries, err := view.ListEntries("/docs", false)
	if err != nil {
		t.Errorf("list: %v", err)
	} else if !reflect.DeepEqual(entries, want.entries) {
		t.Errorf("list = %+v, want %+v", entries, want.entries)
	}
	versions, err := view.Versions("/docs/doc.md")
	if err != nil {
		t.Errorf("versions: %v", err)
	} else {
		got := make([]int, len(versions))
		for i, version := range versions {
			got[i] = version.Version
		}
		if !reflect.DeepEqual(got, want.versions) {
			t.Errorf("versions = %v, want %v", got, want.versions)
		}
	}
	if path, err := view.LookupHash(want.hash); err != nil || path != "/docs/doc.md" {
		t.Errorf("lookup hash = %q, error %v; want /docs/doc.md", path, err)
	}
	if _, err := view.LookupHash(want.missingHash); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing hash error = %v, want not-found", err)
	}
	results, err := view.Lookup(want.lookupQuery, catalog.Options{Scope: "/docs"})
	if err != nil {
		t.Errorf("lookup %q: %v", want.lookupQuery, err)
	} else if len(results) != 1 || results[0].Path != "/docs/doc.md" {
		t.Errorf("lookup %q = %+v, want /docs/doc.md", want.lookupQuery, results)
	}
	results, err = view.Lookup(want.missingLookup, catalog.Options{Scope: "/docs"})
	if err != nil {
		t.Errorf("lookup %q: %v", want.missingLookup, err)
	} else if len(results) != 0 {
		t.Errorf("lookup %q = %+v, want no results", want.missingLookup, results)
	}
	if err := view.VerifyChain("/docs/doc.md"); (err == nil) != want.chainValid {
		t.Errorf("verify chain error = %v, want valid %v", err, want.chainValid)
	}
}

type readViewTestDriver struct{}

func (readViewTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use read-view test connector")
}

type readViewDriverConnector struct {
	state *readViewDriverState
}

func (connector *readViewDriverConnector) Connect(context.Context) (driver.Conn, error) {
	connector.state.mu.Lock()
	connector.state.opens++
	connector.state.mu.Unlock()
	return &readViewDriverConn{state: connector.state}, nil
}

func (*readViewDriverConnector) Driver() driver.Driver {
	return readViewTestDriver{}
}

type readViewDriverState struct {
	mu          sync.Mutex
	beginCtx    context.Context
	queryCtx    context.Context
	options     driver.TxOptions
	beginErr    error
	queryErr    error
	rollbackErr error
	opens       int
	rollbacks   int
}

type readViewDriverSnapshot struct {
	beginCtx  context.Context
	queryCtx  context.Context
	options   driver.TxOptions
	opens     int
	rollbacks int
}

func (state *readViewDriverState) snapshot() readViewDriverSnapshot {
	state.mu.Lock()
	defer state.mu.Unlock()
	return readViewDriverSnapshot{
		beginCtx:  state.beginCtx,
		queryCtx:  state.queryCtx,
		options:   state.options,
		opens:     state.opens,
		rollbacks: state.rollbacks,
	}
}

type readViewDriverConn struct {
	state *readViewDriverState
}

func (conn *readViewDriverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (conn *readViewDriverConn) Close() error { return nil }

func (conn *readViewDriverConn) Begin() (driver.Tx, error) {
	return conn.BeginTx(context.Background(), driver.TxOptions{})
}

func (conn *readViewDriverConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	conn.state.mu.Lock()
	defer conn.state.mu.Unlock()
	conn.state.beginCtx = ctx
	conn.state.options = opts
	if conn.state.beginErr != nil {
		return nil, conn.state.beginErr
	}
	return &readViewDriverTx{state: conn.state}, nil
}

func (conn *readViewDriverConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	conn.state.mu.Lock()
	defer conn.state.mu.Unlock()
	conn.state.queryCtx = ctx
	if conn.state.queryErr != nil {
		return nil, conn.state.queryErr
	}
	return nil, errors.New("query result is not configured")
}

type readViewDriverTx struct {
	state *readViewDriverState
}

func (*readViewDriverTx) Commit() error { return errors.New("commit is not supported") }

func (tx *readViewDriverTx) Rollback() error {
	tx.state.mu.Lock()
	defer tx.state.mu.Unlock()
	tx.state.rollbacks++
	return tx.state.rollbackErr
}

func newReadViewDriverDB(t *testing.T, state *readViewDriverState) *sql.DB {
	t.Helper()
	db := sql.OpenDB(&readViewDriverConnector{state: state})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close read-view test database: %v", err)
		}
	})
	return db
}
