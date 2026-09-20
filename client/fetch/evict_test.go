package fetch

import (
	"context"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// An evicted connection must be closed once its last user is done, and eviction
// must never drop a replacement another goroutine stored under the same host.
func TestEvictClosesOnlyTheExpectedConnection(t *testing.T) {
	addr := startTestServer(t, func(protocol.Request) protocol.Response {
		return protocol.Response{Status: protocol.StatusOK, Body: "# OK\n"}
	})
	c := NewClient(Options{Insecure: true})
	t.Cleanup(c.Close)
	ctx := context.Background()

	first, err := c.acquire(ctx, addr)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := c.acquire(ctx, addr)
	if err != nil {
		t.Fatalf("acquire again: %v", err)
	}
	if first != second {
		t.Fatal("pool handed out two connections for one host")
	}

	c.evict(addr, first)
	c.release(first)
	if first.Context().Err() != nil {
		t.Fatal("connection closed while another request still holds it")
	}
	replacement, err := c.acquire(ctx, addr)
	if err != nil {
		t.Fatalf("acquire replacement: %v", err)
	}
	if replacement == first {
		t.Fatal("evicted connection was handed out again")
	}

	c.release(second)
	select {
	case <-first.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("evicted connection was never closed after its last release")
	}

	// A stale eviction must not remove the replacement.
	c.evict(addr, first)
	again, err := c.acquire(ctx, addr)
	if err != nil {
		t.Fatalf("acquire after stale evict: %v", err)
	}
	if again != replacement {
		t.Fatal("stale eviction dropped the replacement connection")
	}
	c.release(again)
	c.release(replacement)
}
