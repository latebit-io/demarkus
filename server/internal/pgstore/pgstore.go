// Package pgstore is the Postgres-backed implementation of the handler's
// DocumentStore contract. It persists the exact stored version bytes the
// file store writes (store frontmatter + content, see protocol/store
// format.go), so the previous-hash chain, metadata extraction, and
// migration between backends stay byte-identical.
//
// Concurrency: every write transaction row-locks the document's row in
// documents (SELECT ... FOR UPDATE / insert on conflict), so version
// allocation, dedup, and the current-pointer update are atomic. The
// UNIQUE(path, version) primary key is the backstop CAS. Unlike the file
// store, this backend is safe under multiple server processes.
package pgstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	gopath "path"
	"sort"
	"strings"
	"time"

	// Register the pgx database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
)

// schema is applied idempotently at startup. Paths are stored canonical:
// cleaned, no leading slash ("" is the root, "adr/0001.md" a nested doc).
const schema = `
CREATE TABLE IF NOT EXISTS documents (
	path            text PRIMARY KEY,
	current_version integer NOT NULL,
	archived        boolean NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS versions (
	path      text NOT NULL,
	version   integer NOT NULL,
	stored    bytea NOT NULL,
	body_hash text NOT NULL,
	modified  timestamptz NOT NULL,
	PRIMARY KEY (path, version)
);
CREATE INDEX IF NOT EXISTS versions_body_hash_idx ON versions (body_hash);
`

// Store implements the handler's DocumentStore contract against Postgres.
type Store struct {
	db *sql.DB
}

// Open connects to Postgres with the given DSN, applies the schema, and
// returns the store. The caller owns closing via Close.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	s := &Store{db: db}
	if err := s.Init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// NewWithDB wraps an existing connection pool (tests, custom pooling). The
// caller must run Init before use.
func NewWithDB(db *sql.DB) *Store { return &Store{db: db} }

// Init verifies connectivity and applies the idempotent schema.
func (s *Store) Init(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Reset destroys every document and version. It exists for the conformance
// suite, which needs a fresh store per run; never call it on live data.
func (s *Store) Reset(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `TRUNCATE documents, versions`); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return nil
}

// canonical normalizes a request path to the storage key: cleaned, no
// leading slash. "" is the root. ".." segments cannot survive path.Clean
// of a rooted path, so traversal cannot escape the keyspace.
func canonical(reqPath string) string {
	p := gopath.Clean("/" + reqPath)
	return strings.TrimPrefix(p, "/")
}

// Get retrieves a document. version 0 means current. Missing documents and
// directory paths return os.ErrNotExist.
func (s *Store) Get(reqPath string, version int) (*store.Document, error) {
	p := canonical(reqPath)
	var stored []byte
	var modified time.Time
	var ver int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT v.stored, v.modified, v.version
		FROM documents d
		JOIN versions v ON v.path = d.path
		AND v.version = CASE WHEN $2 = 0 THEN d.current_version ELSE $2 END
		WHERE d.path = $1`, p, version).Scan(&stored, &modified, &ver)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", reqPath, err)
	}
	return &store.Document{
		Content:  stored,
		Modified: modified.UTC().Truncate(time.Second),
		Version:  ver,
		Archived: store.IsArchived(stored),
		Metadata: store.ExtractMetadata(stored),
	}, nil
}

// ListDir lists the children of a directory path, mirroring the file
// store's semantics: dot-named entries and "versions" are hidden; archived
// documents are omitted unless includeArchived, and a subdirectory appears
// only when its subtree holds a visible document. A path with no documents
// beneath it (and that is not the root) returns os.ErrNotExist, as does a
// document path.
func (s *Store) ListDir(reqPath string, includeArchived bool) ([]store.DirEntry, error) {
	p := canonical(reqPath)
	prefix := ""
	if p != "" {
		prefix = p + "/"
	}
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT path, archived FROM documents WHERE starts_with(path, $1)`, prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", reqPath, err)
	}
	defer func() { _ = rows.Close() }()

	type child struct {
		isDir   bool
		visible bool
	}
	children := map[string]*child{}
	exists := p == ""
	for rows.Next() {
		var docPath string
		var archived bool
		if err := rows.Scan(&docPath, &archived); err != nil {
			return nil, fmt.Errorf("list %s: %w", reqPath, err)
		}
		exists = true
		rest := strings.TrimPrefix(docPath, prefix)
		visible := includeArchived || !archived
		name, remainder, nested := strings.Cut(rest, "/")
		if hiddenName(name) {
			continue
		}
		if nested {
			// A document deeper down: the first segment is a subdirectory.
			// It is visible only if no deeper segment of this document is
			// hidden (a doc reachable only through dot-names cannot rescue
			// a shell directory, matching the file store's
			// dirHasVisibleEntry walk).
			for seg := range strings.SplitSeq(remainder, "/") {
				if hiddenName(seg) {
					visible = false
					break
				}
			}
		}
		c, ok := children[name]
		if !ok {
			c = &child{isDir: nested}
			children[name] = c
		}
		c.visible = c.visible || visible
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", reqPath, err)
	}
	if !exists {
		// No documents under the prefix: either the path is a plain
		// document (not a directory) or nothing exists there. Both map to
		// os.ErrNotExist, matching the file store.
		return nil, os.ErrNotExist
	}

	names := make([]string, 0, len(children))
	for name, c := range children {
		if c.visible {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	entries := make([]store.DirEntry, 0, len(names))
	for _, name := range names {
		entries = append(entries, store.DirEntry{Name: name, IsDir: children[name].isDir})
	}
	return entries, nil
}

// hiddenName mirrors the file store's isHiddenEntry: dot-names and the
// reserved versions directory never appear in listings.
func hiddenName(name string) bool {
	return strings.HasPrefix(name, ".") || name == "versions"
}

// IsDir reports whether the path is a directory: the root always is; any
// other path is a directory when documents exist beneath it. A document
// path reports false; a missing path returns os.ErrNotExist.
func (s *Store) IsDir(reqPath string) (bool, error) {
	p := canonical(reqPath)
	if p == "" {
		return true, nil
	}
	var hasChildren bool
	err := s.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM documents WHERE starts_with(path, $1))`, p+"/").Scan(&hasChildren)
	if err != nil {
		return false, fmt.Errorf("isdir %s: %w", reqPath, err)
	}
	if hasChildren {
		return true, nil
	}
	var isDoc bool
	err = s.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM documents WHERE path = $1)`, p).Scan(&isDoc)
	if err != nil {
		return false, fmt.Errorf("isdir %s: %w", reqPath, err)
	}
	if isDoc {
		return false, nil
	}
	return false, os.ErrNotExist
}

// Versions returns the version history newest first, or os.ErrNotExist for
// a document with no history.
func (s *Store) Versions(reqPath string) ([]store.VersionInfo, error) {
	p := canonical(reqPath)
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT version, modified FROM versions WHERE path = $1 ORDER BY version DESC`, p)
	if err != nil {
		return nil, fmt.Errorf("versions %s: %w", reqPath, err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.VersionInfo
	for rows.Next() {
		var v store.VersionInfo
		if err := rows.Scan(&v.Version, &v.Modified); err != nil {
			return nil, fmt.Errorf("versions %s: %w", reqPath, err)
		}
		v.Modified = v.Modified.UTC().Truncate(time.Second)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("versions %s: %w", reqPath, err)
	}
	if len(out) == 0 {
		return nil, os.ErrNotExist
	}
	return out, nil
}

// CurrentVersion returns the current version number, or 0 when the document
// does not exist.
func (s *Store) CurrentVersion(reqPath string) int {
	p := canonical(reqPath)
	var cur int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT current_version FROM documents WHERE path = $1`, p).Scan(&cur)
	if err != nil {
		return 0
	}
	return cur
}

// LookupHash resolves a body content hash ("sha256-<hex>") to the request
// path of a current, non-archived document.
func (s *Store) LookupHash(hash string) (string, bool) {
	var p string
	err := s.db.QueryRowContext(context.Background(), `
		SELECT d.path FROM documents d
		JOIN versions v ON v.path = d.path AND v.version = d.current_version
		WHERE v.body_hash = $1 AND NOT d.archived
		ORDER BY d.path LIMIT 1`, hash).Scan(&p)
	if err != nil {
		return "", false
	}
	return "/" + p, true
}

// VerifyChain checks previous-hash integrity across the retained versions,
// oldest to newest, exactly as the file store does.
func (s *Store) VerifyChain(reqPath string) error {
	p := canonical(reqPath)
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT version, stored FROM versions WHERE path = $1 ORDER BY version ASC`, p)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type ver struct {
		n      int
		stored []byte
	}
	var versions []ver
	for rows.Next() {
		var v ver
		if err := rows.Scan(&v.n, &v.stored); err != nil {
			return fmt.Errorf("list versions: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	if len(versions) == 0 {
		return fmt.Errorf("list versions: %w", os.ErrNotExist)
	}
	if len(versions) < 2 {
		return nil
	}
	for i, curr := range versions[1:] {
		prev := versions[i]
		expected := store.ContentHash(prev.stored)
		recorded := store.ExtractPreviousHash(curr.stored)
		if recorded == "" {
			return fmt.Errorf("v%d missing previous-hash", curr.n)
		}
		if recorded != expected {
			return fmt.Errorf("v%d chain broken: previous-hash mismatch (want %s, got %s)",
				curr.n, expected, recorded)
		}
	}
	return nil
}

// Write creates a new version without an optimistic concurrency check.
func (s *Store) Write(reqPath string, content []byte, meta map[string]string) (*store.Document, error) {
	return s.write(reqPath, -1, content, meta)
}

// WriteVersion creates a new version with the file store's expected-version
// semantics: < 0 skips the check, 0 is create-only, > 0 expects that exact
// current version. Violations return store.ErrConflict carrying the actual
// current version.
func (s *Store) WriteVersion(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*store.Document, error) {
	return s.write(reqPath, expectedVersion, content, meta)
}

func (s *Store) write(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*store.Document, error) {
	if int64(len(content)) > protocol.MaxBodyLength {
		return nil, fmt.Errorf("content exceeds size limit")
	}
	if err := store.ValidateMeta(meta); err != nil {
		return nil, err
	}
	p := canonical(reqPath)
	if p == "" {
		return nil, os.ErrNotExist
	}

	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Row lock: serializes concurrent writers on this document. A missing
	// row means version 1; the lock is then taken by the insert below.
	var cur int
	var archived bool
	err = tx.QueryRowContext(ctx, `
		SELECT current_version, archived FROM documents WHERE path = $1 FOR UPDATE`, p).Scan(&cur, &archived)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lock %s: %w", reqPath, err)
	}

	if expectedVersion >= 0 && cur != expectedVersion {
		return &store.Document{Version: cur}, store.ErrConflict
	}

	var tipStored []byte
	if cur > 0 {
		if archived {
			return nil, store.ErrArchived
		}
		var tipModified time.Time
		err = tx.QueryRowContext(ctx, `
			SELECT stored, modified FROM versions WHERE path = $1 AND version = $2`, p, cur).
			Scan(&tipStored, &tipModified)
		if err != nil {
			return nil, fmt.Errorf("read tip %s v%d: %w", reqPath, cur, err)
		}
		if bytes.Equal(store.ExtractBody(tipStored), content) &&
			store.MetaEqual(store.ExtractMetadata(tipStored), meta) {
			return &store.Document{
				Content:  content,
				Modified: tipModified.UTC().Truncate(time.Second),
				Version:  cur,
				Archived: false,
				Metadata: meta,
			}, store.ErrNotModified
		}
	}

	next := cur + 1
	stored, err := store.SerializeVersion(next, tipStored, content, meta)
	if err != nil {
		return nil, err
	}
	if int64(len(stored)) > int64(protocol.MaxBodyLength+store.MaxStoreFrontmatter) {
		return nil, fmt.Errorf("content exceeds size limit")
	}

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO versions (path, version, stored, body_hash, modified)
		VALUES ($1, $2, $3, $4, $5)`, p, next, stored, store.ContentHash(content), now); err != nil {
		return nil, fmt.Errorf("insert v%d: %w", next, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO documents (path, current_version, archived) VALUES ($1, $2, false)
		ON CONFLICT (path) DO UPDATE SET current_version = EXCLUDED.current_version`, p, next); err != nil {
		return nil, fmt.Errorf("update current v%d: %w", next, err)
	}

	doc := &store.Document{
		Content:  content,
		Modified: now,
		Version:  next,
		Archived: false,
		Metadata: meta,
	}

	if keep := store.RetentionValue(meta); keep > 0 {
		if cutoff := next - keep; cutoff >= 1 {
			prune, err := s.pruneVersions(ctx, tx, p, cutoff)
			if err != nil {
				return nil, err
			}
			doc.Prune = prune
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit v%d: %w", next, err)
	}
	return doc, nil
}

// pruneVersions deletes versions <= cutoff inside the write transaction and
// reports the contiguous deleted range, mirroring the file store's
// PruneResult contract. Transactionality makes partial prunes impossible,
// so Err is never set on a returned result.
func (s *Store) pruneVersions(ctx context.Context, tx *sql.Tx, p string, cutoff int) (*store.PruneResult, error) {
	rows, err := tx.QueryContext(ctx, `
		DELETE FROM versions WHERE path = $1 AND version <= $2 RETURNING version`, p, cutoff)
	if err != nil {
		return nil, fmt.Errorf("prune <= v%d: %w", cutoff, err)
	}
	defer func() { _ = rows.Close() }()
	res := &store.PruneResult{}
	deleted := false
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("prune <= v%d: %w", cutoff, err)
		}
		if !deleted || v < res.From {
			res.From = v
		}
		if v > res.To {
			res.To = v
		}
		deleted = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prune <= v%d: %w", cutoff, err)
	}
	if !deleted {
		return nil, nil
	}
	return res, nil
}

// Append reads the document at expectedVersion, appends content (newline
// separated), and writes the result as a new version via WriteVersion.
func (s *Store) Append(reqPath string, expectedVersion int, content []byte, meta map[string]string) (*store.Document, error) {
	if expectedVersion < 1 {
		return nil, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expectedVersion)
	}
	if len(content) == 0 {
		return nil, fmt.Errorf("APPEND requires non-empty content")
	}
	if err := store.ValidateMeta(meta); err != nil {
		return nil, err
	}

	baseDoc, err := s.Get(reqPath, expectedVersion)
	if err != nil {
		current := s.CurrentVersion(reqPath)
		if current > 0 && current != expectedVersion {
			return &store.Document{Version: current}, store.ErrConflict
		}
		return nil, err
	}

	existing := store.ExtractBody(baseDoc.Content)
	combined, err := store.JoinContent(existing, content)
	if err != nil {
		return nil, err
	}
	return s.WriteVersion(reqPath, expectedVersion, combined, meta)
}

// Archive toggles the archived flag: the documents row column and the tip
// version's stored frontmatter, in one transaction. Like the file store it
// modifies the tip in place rather than creating a new version; the hash
// chain stays valid because only successors hash their predecessor and the
// tip has no successor yet.
func (s *Store) Archive(reqPath string, archived bool) error {
	p := canonical(reqPath)
	if p == "" {
		return os.ErrNotExist
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin archive: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var cur int
	err = tx.QueryRowContext(ctx, `
		SELECT current_version FROM documents WHERE path = $1 FOR UPDATE`, p).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return os.ErrNotExist
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", reqPath, err)
	}

	var tipStored []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT stored FROM versions WHERE path = $1 AND version = $2`, p, cur).Scan(&tipStored); err != nil {
		return fmt.Errorf("read tip %s v%d: %w", reqPath, cur, err)
	}
	newStored, err := store.SetArchived(tipStored, archived)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE versions SET stored = $3 WHERE path = $1 AND version = $2`, p, cur, newStored); err != nil {
		return fmt.Errorf("update tip %s v%d: %w", reqPath, cur, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE documents SET archived = $2 WHERE path = $1`, p, archived); err != nil {
		return fmt.Errorf("update archived %s: %w", reqPath, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit archive %s: %w", reqPath, err)
	}
	return nil
}

// WalkCurrent visits every current, non-archived document, matching the
// file store's WalkCurrent so startup catalog builds work on either
// backend.
func (s *Store) WalkCurrent(fn func(store.CurrentDoc) error) error {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT d.path, v.stored, v.modified
		FROM documents d
		JOIN versions v ON v.path = d.path AND v.version = d.current_version
		WHERE NOT d.archived
		ORDER BY d.path`)
	if err != nil {
		return fmt.Errorf("walk current: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		var stored []byte
		var modified time.Time
		if err := rows.Scan(&p, &stored, &modified); err != nil {
			return fmt.Errorf("walk current: %w", err)
		}
		err := fn(store.CurrentDoc{
			Path:     "/" + p,
			Body:     store.ExtractBody(stored),
			Metadata: store.ExtractMetadata(stored),
			Modified: modified.UTC().Truncate(time.Second),
		})
		if err != nil {
			return err
		}
	}
	return rows.Err()
}
