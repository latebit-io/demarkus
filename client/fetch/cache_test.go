package fetch

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// memoryCache is a ResponseCache that lives outside this module's internals,
// as a tool's own cache would.
type memoryCache struct {
	mu      sync.Mutex
	entries map[string]*CachedResponse
	getErr  error
	puts    int
}

func (m *memoryCache) Get(host, path, verb string) (*CachedResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.entries[verb+" "+host+path], nil
}

func (m *memoryCache) Put(host, path, verb string, resp protocol.Response) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[string]*CachedResponse)
	}
	m.entries[verb+" "+host+path] = &CachedResponse{Response: resp}
	m.puts++
	return nil
}

func TestFetchServesAndRevalidatesThroughTheCache(t *testing.T) {
	const etag = "etag-1"
	var conditional, unconditional atomic.Int32
	host := startTestServer(t, func(req protocol.Request) protocol.Response {
		if req.Metadata["if-none-match"] == etag {
			conditional.Add(1)
			return protocol.Response{Status: protocol.StatusNotModified, Metadata: map[string]string{"etag": etag}}
		}
		unconditional.Add(1)
		return protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"etag": etag}, Body: "# Doc\n"}
	})
	store := &memoryCache{}
	c := NewClient(Options{Insecure: true, Cache: store})
	defer c.Close()

	first, err := c.Fetch(t.Context(), FetchRequest{Host: host, Path: "/doc.md"})
	if err != nil || first.FromCache || first.Response.Body != "# Doc\n" {
		t.Fatalf("first fetch = %+v, %v, want a fresh body", first, err)
	}
	second, err := c.Fetch(t.Context(), FetchRequest{Host: host, Path: "/doc.md"})
	if err != nil || !second.FromCache || second.Response.Body != "# Doc\n" {
		t.Fatalf("second fetch = %+v, %v, want the cached body after not-modified", second, err)
	}
	if unconditional.Load() != 1 || conditional.Load() != 1 || store.puts != 1 {
		t.Errorf("unconditional=%d conditional=%d puts=%d, want 1 1 1", unconditional.Load(), conditional.Load(), store.puts)
	}

	// A token or a request option bypasses the cache: neither is in its key.
	for name, req := range map[string]FetchRequest{
		"token":       {Host: host, Path: "/doc.md", Token: "secret"},
		"conditional": {Host: host, Path: "/doc.md", IfNoneMatch: "other"},
	} {
		got, err := c.Fetch(t.Context(), req)
		if err != nil || got.FromCache {
			t.Errorf("%s fetch = %+v, %v, want it to bypass the cache", name, got, err)
		}
	}
	if store.puts != 1 {
		t.Errorf("puts = %d, want bypassing requests to store nothing", store.puts)
	}
}

// An unreadable cache is a miss, never a failed read.
func TestFetchTreatsACacheReadErrorAsAMiss(t *testing.T) {
	host := startTestServer(t, func(protocol.Request) protocol.Response {
		return protocol.Response{Status: protocol.StatusOK, Body: "# Doc\n"}
	})
	c := NewClient(Options{Insecure: true, Cache: &memoryCache{getErr: errors.New("disk gone")}})
	defer c.Close()
	got, err := c.Fetch(t.Context(), FetchRequest{Host: host, Path: "/doc.md"})
	if err != nil || got.Response.Body != "# Doc\n" {
		t.Fatalf("fetch = %+v, %v, want the server's answer", got, err)
	}
}
