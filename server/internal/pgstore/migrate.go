package pgstore

// Migration surface: the Postgres half of store.Migrator (see
// protocol/store/migrate.go), moving raw stored bytes between backends.

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// ExportDocs visits every document, archived included, in path order with
// its history oldest-first and stored bytes verbatim, bounded by the
// caller's ctx. reqPath is the request spelling, matching the file store.
func (s *Store) ExportDocs(ctx context.Context, fn func(reqPath string, versions []store.StoredVersion) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, version, stored, modified FROM versions
		ORDER BY path, version`)
	if err != nil {
		return fmt.Errorf("export versions: %w", err)
	}
	// Close error is redundant: rows.Err() below reports iteration failures.
	defer func() { _ = rows.Close() }()

	var curPath string
	var cur []store.StoredVersion
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		err := fn("/"+curPath, cur)
		// The callback may retain the slice (Migrator contract); start a
		// fresh backing array for the next document.
		cur = nil
		return err
	}
	for rows.Next() {
		var v store.StoredVersion
		var p string
		if err := rows.Scan(&p, &v.Version, &v.Stored, &v.Modified); err != nil {
			return fmt.Errorf("export scan: %w", err)
		}
		if p != curPath {
			if err := flush(); err != nil {
				return err
			}
			curPath = p
		}
		cur = append(cur, v)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export rows: %w", err)
	}
	return flush()
}

// ImportDoc inserts a document's rows byte-for-byte in one transaction,
// bounded by the caller's ctx: versions verbatim, the documents row and the
// catalog row derived from the tip, as a live write would.
func (s *Store) ImportDoc(ctx context.Context, reqPath string, versions []store.StoredVersion) error {
	p, err := store.ValidateImport(reqPath, versions)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin import: %w", err)
	}
	// No-op (ErrTxDone) after a successful Commit; the safety net for every
	// early return below.
	defer func() { _ = tx.Rollback() }()

	newest := versions[len(versions)-1]
	// The documents PK doubles as the exists check: a duplicate import
	// surfaces as a unique violation, folded into ErrExist.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO documents (path, current_version, archived) VALUES ($1, $2, $3)`,
		p, newest.Version, store.IsArchived(newest.Stored)); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return fmt.Errorf("import %s: %w", reqPath, os.ErrExist)
		}
		return fmt.Errorf("import %s: %w", reqPath, err)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO versions (path, version, stored, body_hash, modified)
		VALUES ($1, $2, $3, $4, $5)`)
	if err != nil {
		return fmt.Errorf("import %s: %w", reqPath, err)
	}
	// Statement lives on the tx; Rollback/Commit releases it either way.
	defer func() { _ = stmt.Close() }()
	for _, v := range versions {
		if _, err := stmt.ExecContext(ctx, p, v.Version, v.Stored,
			store.ContentHash(store.ExtractBody(v.Stored)), v.Modified); err != nil {
			return fmt.Errorf("import %s v%d: %w", reqPath, v.Version, err)
		}
	}

	tipMeta := store.ExtractMetadata(newest.Stored)
	entry := catalog.FromDocument("/"+p, tipMeta, store.ExtractBody(newest.Stored), newest.Modified.UTC())
	if err := upsertCatalogRow(ctx, tx, p, entry); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import %s: %w", reqPath, err)
	}
	return nil
}
