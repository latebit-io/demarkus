package pgstore_test

import (
	"context"
	"os"
	"testing"

	"github.com/latebit-io/demarkus/server/internal/handler"
	"github.com/latebit-io/demarkus/server/internal/pgstore"
	"github.com/latebit-io/demarkus/server/internal/storetest"
)

// TestPostgresConformance runs the shared DocumentStore conformance suite
// against Postgres. Gated on DEMARKUS_TEST_PG_DSN so environments without a
// database skip instead of fail, e.g.:
//
//	DEMARKUS_TEST_PG_DSN="postgres://postgres:postgres@localhost:5432/demarkus_test" go test ./internal/pgstore/
func TestPostgresConformance(t *testing.T) {
	dsn := os.Getenv("DEMARKUS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("DEMARKUS_TEST_PG_DSN not set; skipping Postgres conformance")
	}
	s, err := pgstore.Open(dsn, nil)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	storetest.RunConformance(t, func(t *testing.T) handler.DocumentStore {
		if err := s.Reset(context.Background()); err != nil {
			t.Fatalf("reset: %v", err)
		}
		return s
	})
}
