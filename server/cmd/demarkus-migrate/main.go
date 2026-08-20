// demarkus-migrate copies a world's full version history between store
// backends, byte-for-byte: file -> postgres or postgres -> file. The
// destination must be empty; a verify pass re-exports the destination and
// checks every stored version against hashes recorded during the copy.
//
// Usage:
//
//	demarkus-migrate -from file -to postgres -root /srv/site -pg-dsn "$DSN"
//	demarkus-migrate -from postgres -to file -root /srv/new -pg-dsn "$DSN"
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/logging"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
)

func main() {
	from := flag.String("from", "", "source backend: file or postgres")
	to := flag.String("to", "", "destination backend: file or postgres")
	root := flag.String("root", "", "content directory for the file side (overrides DEMARKUS_ROOT)")
	pgDSN := flag.String("pg-dsn", "", "Postgres connection string for the postgres side (overrides DEMARKUS_PG_DSN)")
	verify := flag.Bool("verify", true, "after copying, re-export the destination and compare every stored version")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-migrate -from file|postgres -to file|postgres [options]\n\n")
		fmt.Fprintf(os.Stderr, "Copy a world's full version history between store backends, byte-for-byte.\n")
		fmt.Fprintf(os.Stderr, "The destination must be empty. Stop the server before migrating.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	logger := logging.New(os.Getenv("DEMARKUS_LOG_FORMAT"), os.Getenv("DEMARKUS_LOG_LEVEL"), nil)
	if err := run(*from, *to, *root, *pgDSN, *verify, logger); err != nil {
		logger.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(from, to, root, pgDSN string, verify bool, logger *slog.Logger) error {
	if from == to || !validBackend(from) || !validBackend(to) {
		return fmt.Errorf("need -from and -to as file|postgres and different (got %q -> %q)", from, to)
	}
	if root == "" {
		root = os.Getenv("DEMARKUS_ROOT")
	}
	if pgDSN == "" {
		pgDSN = os.Getenv("DEMARKUS_PG_DSN")
	}
	if root == "" {
		return fmt.Errorf("-root (or DEMARKUS_ROOT) is required for the file side")
	}
	if pgDSN == "" {
		return fmt.Errorf("-pg-dsn (or DEMARKUS_PG_DSN) is required for the postgres side")
	}

	fs, err := store.Open(root)
	if err != nil {
		return fmt.Errorf("open file store: %w", err)
	}
	pg, err := pgstore.Open(pgDSN, logger)
	if err != nil {
		return fmt.Errorf("open postgres store: %w", err)
	}
	defer func() {
		if err := pg.Close(); err != nil {
			logger.Warn("postgres close failed", "error", err)
		}
	}()

	backends := map[string]store.Migrator{"file": fs, "postgres": pg}
	src, dst := backends[from], backends[to]

	if err := requireEmpty(dst, to); err != nil {
		return err
	}

	logger.Info("copying", "from", from, "to", to)
	// Per-version summaries recorded during the copy: the verify pass then
	// reads only the destination, and memory stays ~50 bytes per version.
	want := map[string][]versionSummary{}
	var docs, versions int
	if err := src.ExportDocs(func(p string, vs []store.StoredVersion) error {
		docs++
		versions += len(vs)
		if verify {
			want[p] = summarize(vs)
		}
		return dst.ImportDoc(p, vs)
	}); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	logger.Info("copied", "documents", docs, "versions", versions)

	if verify {
		if err := verifyDst(dst, want); err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		logger.Info("verified", "documents", docs, "versions", versions)
	}
	return nil
}

func validBackend(b string) bool { return b == "file" || b == "postgres" }

// errNonEmpty aborts the emptiness probe on the first exported document.
var errNonEmpty = fmt.Errorf("destination is not empty; migrate into a fresh root/database")

func requireEmpty(dst store.Migrator, name string) error {
	err := dst.ExportDocs(func(string, []store.StoredVersion) error { return errNonEmpty })
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// versionSummary is what verify holds per version instead of raw bytes.
type versionSummary struct {
	version  int
	hash     string
	modified time.Time
}

func summarize(vs []store.StoredVersion) []versionSummary {
	out := make([]versionSummary, len(vs))
	for i, v := range vs {
		out[i] = versionSummary{v.Version, store.ContentHash(v.Stored), v.Modified.UTC().Truncate(time.Second)}
	}
	return out
}

// verifyDst re-exports the destination and compares every stored version's
// hash and modified time (to the second, the protocol's precision) against
// the summaries recorded from the source during the copy.
func verifyDst(dst store.Migrator, want map[string][]versionSummary) error {
	if err := dst.ExportDocs(func(p string, vs []store.StoredVersion) error {
		wv, ok := want[p]
		if !ok {
			return fmt.Errorf("%s: not in source", p)
		}
		delete(want, p)
		got := summarize(vs)
		if len(got) != len(wv) {
			return fmt.Errorf("%s: %d versions, want %d", p, len(got), len(wv))
		}
		for i := range wv {
			if got[i] != wv[i] {
				return fmt.Errorf("%s v%d: stored bytes or modified differ", p, wv[i].version)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(want) != 0 {
		return fmt.Errorf("%d source documents missing from destination", len(want))
	}
	return nil
}
