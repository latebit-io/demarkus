package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// This file is the Postgres implementation of the handler's LookupCatalog
// seam. Catalog rows are written inside the same transaction as the document
// write (see upsertCatalogRow calls in write), so every replica sees a
// LOOKUP catalog exactly as current as the store itself; there is no per-pod
// in-RAM index to diverge.
//
// Parity with catalog.Catalog is split deliberately:
//   - All lowercasing happens in Go at write time (tags_lower, title_lower),
//     because Postgres lower() and Go strings.ToLower disagree on unicode
//     edge cases. The SQL query only ever compares Go-lowered values against
//     Go-lowered query terms (catalog.Tokenize).
//   - Match scoring, archived exclusion, scope, and ordering run in SQL.
//   - Filter predicates run in Go through catalog.MatchesAll, the same code
//     the in-memory catalog uses, so filter semantics cannot diverge.

// upsertCatalogRow writes the catalog row for a document tip inside the
// caller's write transaction. entry is derived via catalog.FromDocument, the
// same derivation the handler performs for the in-memory catalog. Archived
// state is not stored here: the lookup query joins documents.archived, so
// archive/unarchive need no catalog writes and unarchive restores the entry.
func upsertCatalogRow(ctx context.Context, tx *sql.Tx, p string, entry *catalog.Entry) error {
	args, err := catalogRowArgs(p, entry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO catalog (path, tags, tags_lower, importance, title, title_lower, meta, modified)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (path) DO UPDATE SET
			tags = EXCLUDED.tags, tags_lower = EXCLUDED.tags_lower,
			importance = EXCLUDED.importance, title = EXCLUDED.title,
			title_lower = EXCLUDED.title_lower, meta = EXCLUDED.meta,
			modified = EXCLUDED.modified`, args...)
	if err != nil {
		return fmt.Errorf("catalog row %s: %w", p, err)
	}
	return nil
}

// insertCatalogRowIfAbsent is the backfill variant of upsertCatalogRow: it
// never overwrites, so a row created by a concurrent live write (which is
// newer than the tip the backfill read) always wins.
func insertCatalogRowIfAbsent(ctx context.Context, tx *sql.Tx, p string, entry *catalog.Entry) error {
	args, err := catalogRowArgs(p, entry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO catalog (path, tags, tags_lower, importance, title, title_lower, meta, modified)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (path) DO NOTHING`, args...)
	if err != nil {
		return fmt.Errorf("catalog row %s: %w", p, err)
	}
	return nil
}

// catalogRowArgs encodes an entry as the shared column values of the two
// catalog INSERT statements. All lowercasing happens here, in Go.
func catalogRowArgs(p string, entry *catalog.Entry) ([]any, error) {
	tagsJSON, err := jsonArray(entry.Tags)
	if err != nil {
		return nil, fmt.Errorf("catalog row %s: %w", p, err)
	}
	lower := make([]string, len(entry.Tags))
	for i, tag := range entry.Tags {
		lower[i] = strings.ToLower(tag)
	}
	tagsLowerJSON, err := jsonArray(lower)
	if err != nil {
		return nil, fmt.Errorf("catalog row %s: %w", p, err)
	}
	metaJSON, err := jsonObject(entry.Metadata)
	if err != nil {
		return nil, fmt.Errorf("catalog row %s: %w", p, err)
	}
	return []any{p, tagsJSON, tagsLowerJSON, entry.Importance,
		entry.Title, strings.ToLower(entry.Title), metaJSON, entry.Modified}, nil
}

// jsonArray marshals a string slice as a jsonb array, never null: the lookup
// query feeds the value to jsonb_array_elements_text, which rejects scalars.
func jsonArray(ss []string) ([]byte, error) {
	if ss == nil {
		ss = []string{}
	}
	return json.Marshal(ss)
}

// jsonObject marshals a metadata map as a jsonb object, never null.
func jsonObject(m map[string]string) ([]byte, error) {
	if m == nil {
		m = map[string]string{}
	}
	return json.Marshal(m)
}

// backfillCatalog inserts catalog rows for document tips that have none. It
// runs on every Init: the first start after this table was introduced
// backfills the whole world (the additive migration), and later starts
// reconcile stragglers, e.g. documents written by an old binary during a
// rolling deploy, which upserted no catalog rows. In the steady state the
// query matches nothing. Archived documents get rows too; their entries stay
// invisible to Lookup (the query excludes archived) but must exist so a
// later unarchive restores them. ON CONFLICT DO NOTHING makes concurrent
// replica startups idempotent and never overwrites a row a live write
// created in the meantime.
func (s *Store) backfillCatalog(ctx context.Context) error {
	type tip struct {
		path     string
		stored   []byte
		modified time.Time
	}
	// Materialize before writing: database/sql allows one active statement
	// per transaction, so reading and inserting cannot interleave.
	var tips []tip
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.path, v.stored, v.modified
		FROM documents d
		JOIN versions v ON v.path = d.path AND v.version = d.current_version
		LEFT JOIN catalog c ON c.path = d.path
		WHERE c.path IS NULL`)
	if err != nil {
		return fmt.Errorf("catalog backfill read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t tip
		if err := rows.Scan(&t.path, &t.stored, &t.modified); err != nil {
			return fmt.Errorf("catalog backfill read: %w", err)
		}
		tips = append(tips, t)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog backfill read: %w", err)
	}
	if len(tips) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range tips {
		entry := catalog.FromDocument("/"+t.path,
			store.ExtractMetadata(t.stored), store.ExtractBody(t.stored),
			t.modified.UTC().Truncate(time.Second))
		if err := insertCatalogRowIfAbsent(ctx, tx, t.path, entry); err != nil {
			return fmt.Errorf("catalog backfill: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog backfill: %w", err)
	}
	s.logger.Info("lookup catalog backfilled", "entries", len(tips))
	return nil
}

// Lookup implements the handler's LookupCatalog seam against the catalog
// table, replicating catalog.Catalog.Lookup exactly: score is the count of
// distinct query terms matching a tag (case-insensitive, exact) or the
// non-empty title (case-insensitive substring); ordering is score desc,
// importance desc, modified desc, path asc; "*" matches everything under
// the scope with score zero. Filter predicates and the Max cap are applied
// in Go via the shared catalog code.
func (s *Store) Lookup(query string, opts catalog.Options) ([]catalog.Result, error) {
	matchAll := strings.TrimSpace(query) == "*"
	terms := catalog.Tokenize(query)
	if matchAll {
		// Score must stay zero for every row so ordering reduces to
		// importance, exactly like the in-memory match-all path.
		terms = nil
	} else if len(terms) == 0 {
		return nil, nil
	}
	termsJSON, err := jsonArray(terms)
	if err != nil {
		return nil, fmt.Errorf("lookup terms: %w", err)
	}

	sel := `
		SELECT c.path, c.tags, c.importance, c.title, c.meta, c.modified,
			(SELECT count(*)
			 FROM jsonb_array_elements_text($1::jsonb) AS q(term)
			 WHERE EXISTS (
				SELECT 1 FROM jsonb_array_elements_text(c.tags_lower) AS tg(tag)
				WHERE tg.tag = q.term)
			   OR (c.title_lower <> '' AND strpos(c.title_lower, q.term) > 0)
			)::int AS score
		FROM catalog c
		JOIN documents d ON d.path = c.path
		WHERE NOT d.archived`
	args := []any{termsJSON}
	if scope := canonical(opts.Scope); scope != "" {
		prefix := scope + "/"
		sel += ` AND (c.path = $2 OR (c.path >= $3 AND c.path < $4))`
		args = append(args, scope, prefix, prefixUpperBound(prefix))
	}
	q := `SELECT * FROM (` + sel + `) m`
	if !matchAll {
		q += ` WHERE m.score > 0`
	}
	// C-collated path asc equals the in-memory Go byte-wise tiebreak.
	q += ` ORDER BY m.score DESC, m.importance DESC, m.modified DESC, m.path ASC`

	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []catalog.Result
	for rows.Next() {
		r, err := scanLookupRow(rows)
		if err != nil {
			return nil, fmt.Errorf("lookup: %w", err)
		}
		if !catalog.MatchesAll(&r.Entry, opts.Filter) {
			continue
		}
		results = append(results, r)
		if opts.Max > 0 && len(results) >= opts.Max {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	return results, nil
}

// scanLookupRow decodes one lookup result row into the catalog result shape.
func scanLookupRow(rows *sql.Rows) (catalog.Result, error) {
	var r catalog.Result
	var p string
	var tagsJSON, metaJSON []byte
	var modified time.Time
	if err := rows.Scan(&p, &tagsJSON, &r.Importance, &r.Title, &metaJSON, &modified, &r.Score); err != nil {
		return r, err
	}
	if err := json.Unmarshal(tagsJSON, &r.Tags); err != nil {
		return r, fmt.Errorf("decode tags for %s: %w", p, err)
	}
	if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
		return r, fmt.Errorf("decode meta for %s: %w", p, err)
	}
	if len(r.Tags) == 0 {
		r.Tags = nil
	}
	r.Path = "/" + p
	r.Modified = modified.UTC().Truncate(time.Second)
	return r, nil
}

// Put implements the handler's LookupCatalog seam as a no-op: the catalog
// row was already upserted inside the write transaction that produced the
// document, which is the whole point of this backend. Doing it again here
// would race other replicas' writes.
func (s *Store) Put(_ string, _ map[string]string, _ []byte, _ time.Time) {}

// Remove implements the handler's LookupCatalog seam as a no-op: the row is
// kept and Lookup excludes archived documents via the documents join, so the
// archive transaction that flipped the flag already hid the entry (and a
// later unarchive restores it) transactionally.
func (s *Store) Remove(_ string) {}
