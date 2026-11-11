package fetch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

func okServer(t *testing.T) string {
	t.Helper()
	return startTestServer(t, func(protocol.Request) protocol.Response {
		return protocol.Response{Status: protocol.StatusOK, Body: "# OK\n"}
	})
}

func (c *Client) pooled() (conns, parked, inflight int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.conns), len(c.parked), len(c.inflight)
}

// A pooled connection nothing uses is closed after the idle timeout and its
// pool entries go with it; one in use is left alone however long it is held.
func TestIdleConnectionIsClosedAndForgotten(t *testing.T) {
	addr := okServer(t)
	c := NewClient(Options{Insecure: true, IdleTimeout: 40 * time.Millisecond})
	t.Cleanup(c.Close)
	ctx := context.Background()

	held, err := c.acquire(ctx, addr)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if held.Context().Err() != nil {
		t.Fatal("a connection in use was closed as idle")
	}
	c.release(addr, held)
	select {
	case <-held.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the idle connection was never closed")
	}
	waitFor(t, "the pool to forget the idle connection", func() bool {
		conns, parked, inflight := c.pooled()
		return conns+parked+inflight == 0
	})
	if _, err := c.Fetch(ctx, FetchRequest{Host: addr, Path: "/a.md"}); err != nil {
		t.Fatalf("fetch after the idle close: %v", err)
	}
}

// Past the cap the connection unused longest is closed at once, so a crawl
// across many hosts leaves a bounded pool behind.
func TestIdleCapClosesTheLeastRecentlyUsed(t *testing.T) {
	first, second := okServer(t), okServer(t)
	c := NewClient(Options{Insecure: true, MaxIdleConns: 1, IdleTimeout: -1})
	t.Cleanup(c.Close)
	ctx := context.Background()
	old, err := c.acquire(ctx, first)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	c.release(first, old)
	if _, err := c.Fetch(ctx, FetchRequest{Host: second, Path: "/a.md"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	select {
	case <-old.Context().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the connection over the idle cap was never closed")
	}
	if conns, parked, _ := c.pooled(); conns != 1 || parked != 1 {
		t.Errorf("pool holds %d connections, %d idle; want the newest one", conns, parked)
	}
}

// What the pool keeps tracks the connections in use or recently used, not
// every connection the client ever opened.
func TestPoolRetainsNoExpiredConnections(t *testing.T) {
	addr := okServer(t)
	c := NewClient(Options{Insecure: true, IdleTimeout: 10 * time.Millisecond})
	t.Cleanup(c.Close)
	ctx := context.Background()
	cycle := func() {
		if _, err := c.Fetch(ctx, FetchRequest{Host: addr, Path: "/a.md"}); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		waitFor(t, "the idle close", func() bool {
			conns, parked, inflight := c.pooled()
			return conns+parked+inflight == 0
		})
	}
	for range 4 {
		cycle() // warm up what one cycle allocates once
	}
	grew := memtest.Retained(func() {
		for range 40 {
			cycle()
		}
	})
	runtime.KeepAlive(c)
	t.Logf("the client retains %d bytes after 40 more connections", grew)
	// One live connection is well over 100 KB; forty kept would be megabytes.
	if grew > 512<<10 {
		t.Errorf("the client grew %d bytes over 40 expired connections", grew)
	}
}

// failingCache misses every read and refuses every write.
type failingCache struct{}

func (failingCache) Get(string, string, string) (*CachedResponse, error) { return nil, nil }
func (failingCache) Put(string, string, string, protocol.Response) error {
	return errors.New("disk full")
}

// The client reports through the logger it was given, not the process's.
func TestClientLogsThroughItsOptions(t *testing.T) {
	var got []string
	c := NewClient(Options{Insecure: true, Cache: failingCache{}, Logf: func(format string, args ...any) {
		got = append(got, fmt.Sprintf(format, args...))
	}})
	t.Cleanup(c.Close)
	if _, err := c.Fetch(context.Background(), FetchRequest{Host: okServer(t), Path: "/a.md"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0] != "[WARN] cache write: disk full" {
		t.Fatalf("logged %q, want the cache failure on the injected logger", got)
	}
}
