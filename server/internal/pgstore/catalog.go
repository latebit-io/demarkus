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

// Postgres LookupCatalog: rows ride the document write tx, so every
// replica's LOOKUP is as current as the store. Lowercasing happens in Go at
// write time (Postgres lower() diverges on unicode); filters reuse shared
// catalog code so they cannot diverge.

// upsertCatalogRow writes a document tip's catalog row inside the caller's
// write transaction. Archived state is not stored: Lookup joins
// documents.archived, so archive/unarchive need no catalog writes.
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

// insertCatalogRowIfAbsent is the backfill variant: it never overwrites, so
// a concurrent live write's newer row always wins.
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

// catalogRowArgs encodes an entry as the column values shared by both
// catalog INSERTs. All lowercasing happens here, in Go.
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

// jsonArray marshals as a jsonb array, never null:
// jsonb_array_elements_text rejects scalars.
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

// backfillBatchSize bounds tips (with full stored blobs) held in memory per
// backfill round.
const backfillBatchSize = 100

// backfillCatalog inserts catalog rows for tips lacking them (pre-catalog
// migration, rolling-deploy stragglers, archived docs included). Per-batch
// commits let an interrupted startup resume where it left off.
func (s *Store) backfillCatalog(ctx context.Context) error {
	total := 0
	for {
		n, err := s.backfillBatch(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total > 0 {
		s.logger.Info("lookup catalog backfilled", "entries", total)
	}
	return nil
}

// backfillBatch fills one batch of missing rows; the WHERE c.path IS NULL
// predicate advances past committed batches, so no cursor is needed.
// ON CONFLICT DO NOTHING keeps concurrent replica startups idempotent.
func (s *Store) backfillBatch(ctx context.Context) (int, error) {
	type tip struct {
		path     string
		stored   []byte
		modified time.Time
	}
	// Materialize first: database/sql allows one active statement per tx.
	var tips []tip
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.path, v.stored, v.modified
		FROM documents d
		JOIN versions v ON v.path = d.path AND v.version = d.current_version
		LEFT JOIN catalog c ON c.path = d.path
		WHERE c.path IS NULL
		ORDER BY d.path
		LIMIT $1`, backfillBatchSize)
	if err != nil {
		return 0, fmt.Errorf("catalog backfill read: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t tip
		if err := rows.Scan(&t.path, &t.stored, &t.modified); err != nil {
			return 0, fmt.Errorf("catalog backfill read: %w", err)
		}
		tips = append(tips, t)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("catalog backfill read: %w", err)
	}
	if len(tips) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("catalog backfill: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, t := range tips {
		entry := catalog.FromDocument("/"+t.path,
			store.ExtractMetadata(t.stored), store.ExtractBody(t.stored),
			t.modified.UTC().Truncate(time.Second))
		if err := insertCatalogRowIfAbsent(ctx, tx, t.path, entry); err != nil {
			return 0, fmt.Errorf("catalog backfill: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("catalog backfill: %w", err)
	}
	return len(tips), nil
}

// Lookup replicates catalog.Catalog.Lookup's semantics exactly; the
// storetest LOOKUP conformance suite is the contract. Filters and the Max
// cap run in Go via shared catalog code.
func (s *Store) Lookup(query string, opts catalog.Options) ([]catalog.Result, error) {
	matchAll := strings.TrimSpace(query) == "*"
	terms := catalog.Tokenize(query)
	if matchAll {
		// Zero terms keep every score zero: match-all ordering is importance.
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
	// Same scope normalization as the in-memory catalog, minus the leading
	// slash stored paths lack; an uncleaned scope matches nothing on both.
	if scope := catalog.NormalizeScope(opts.Scope); scope != "/" {
		key := strings.TrimPrefix(scope, "/")
		prefix := key + "/"
		sel += ` AND (c.path = $2 OR (c.path >= $3 AND c.path < $4))`
		args = append(args, key, prefix, prefixUpperBound(prefix))
	}
	q := `SELECT * FROM (` + sel + `) m`
	if !matchAll {
		q += ` WHERE m.score > 0`
	}
	// C-collated path asc equals the in-memory byte-wise tiebreak.
	q += ` ORDER BY m.score DESC, m.importance DESC, m.modified DESC, m.path ASC`
	// Filters run in Go, so SQL may truncate only when none are present.
	if len(opts.Filter) == 0 && opts.Max > 0 {
		args = append(args, opts.Max)
		q += fmt.Sprintf(` LIMIT $%d`, len(args))
	}

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

// Put is a no-op: the write transaction already upserted the row; repeating
// it here would race other replicas' writes.
func (s *Store) Put(_ string, _ map[string]string, _ []byte, _ time.Time) {}

// Remove is a no-op: Lookup excludes archived docs via the documents join,
// so the archive transaction already hid the entry.
func (s *Store) Remove(_ string) {}
