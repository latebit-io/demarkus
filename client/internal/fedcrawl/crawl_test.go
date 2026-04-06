package fedcrawl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
)

// mockClient implements FetchClient for testing.
type mockClient struct {
	mu    sync.Mutex
	pages map[string]mockPage // keyed by "host/path"
	lists map[string]mockPage // keyed by "host/path" for LIST results
	calls []string
}

type mockPage struct {
	status   string
	body     string
	metadata map[string]string
}

func newMockClient() *mockClient {
	return &mockClient{
		pages: make(map[string]mockPage),
		lists: make(map[string]mockPage),
	}
}

func (m *mockClient) addDoc(host, path, body, hash string) {
	m.pages[host+path] = mockPage{
		status: protocol.StatusOK,
		body:   body,
		metadata: map[string]string{
			"etag":         "etag-" + path,
			"content-hash": hash,
		},
	}
}

func (m *mockClient) addList(host, path, entries string) {
	m.lists[host+path] = mockPage{
		status: protocol.StatusOK,
		body:   entries,
	}
}

func (m *mockClient) Fetch(host, path, _ string) (fetch.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, "FETCH "+host+path)
	m.mu.Unlock()

	p, ok := m.pages[host+path]
	if !ok {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	return fetch.Result{Response: protocol.Response{
		Status:   p.status,
		Body:     p.body,
		Metadata: p.metadata,
	}}, nil
}

func (m *mockClient) List(host, path, _ string) (fetch.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, "LIST "+host+path)
	m.mu.Unlock()

	p, ok := m.lists[host+path]
	if !ok {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	return fetch.Result{Response: protocol.Response{
		Status:   p.status,
		Body:     p.body,
		Metadata: p.metadata,
	}}, nil
}

func TestCrawlerSingleServer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [index.md](index.md)\n- [about.md](about.md)\n")
	client.addDoc("example.com:6309", "/index.md", "# Home\n\nContent.", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/about.md", "# About\n\nInfo.", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ServersDiscovered != 1 {
		t.Errorf("ServersDiscovered = %d, want 1", result.ServersDiscovered)
	}
	if result.DocumentsCrawled != 2 {
		t.Errorf("DocumentsCrawled = %d, want 2", result.DocumentsCrawled)
	}
	if result.HashesCollected != 2 {
		t.Errorf("HashesCollected = %d, want 2", result.HashesCollected)
	}

	hashes := crawler.Hashes()
	if len(hashes) != 2 {
		t.Errorf("len(Hashes()) = %d, want 2", len(hashes))
	}
}

func TestCrawlerMultiServer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://server1.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()

	// Server 1 has a link to server 2.
	client.addList("server1.com:6309", "/", "- [index.md](index.md)\n")
	client.addDoc("server1.com:6309", "/index.md", "# Home\n\nSee [other](mark://server2.com/other.md).", "sha256-1111111111111111111111111111111111111111111111111111111111111111")

	// Server 2.
	client.addList("server2.com:6309", "/", "- [other.md](other.md)\n")
	client.addDoc("server2.com:6309", "/other.md", "# Other\n\nContent.", "sha256-2222222222222222222222222222222222222222222222222222222222222222")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ServersDiscovered != 2 {
		t.Errorf("ServersDiscovered = %d, want 2", result.ServersDiscovered)
	}
	if result.DocumentsCrawled != 2 {
		t.Errorf("DocumentsCrawled = %d, want 2", result.DocumentsCrawled)
	}
}

func TestCrawlerMaxDocuments(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 1

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [a.md](a.md)\n- [b.md](b.md)\n")
	client.addDoc("example.com:6309", "/a.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/b.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.DocumentsCrawled > 1 {
		t.Errorf("DocumentsCrawled = %d, should be capped at 1", result.DocumentsCrawled)
	}
}

func TestCrawlerMaxServers(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://server1.com"}
	cfg.Crawl.MaxServers = 1

	client := newMockClient()
	client.addList("server1.com:6309", "/", "- [index.md](index.md)\n")
	client.addDoc("server1.com:6309", "/index.md", "See [other](mark://server2.com/other.md).", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addList("server2.com:6309", "/", "- [other.md](other.md)\n")
	client.addDoc("server2.com:6309", "/other.md", "Content.", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.ServersDiscovered > 1 {
		t.Errorf("ServersDiscovered = %d, should be capped at 1", result.ServersDiscovered)
	}
}

func TestCrawlerCancellation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 1000 // High limit

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [index.md](index.md)\n")
	client.addDoc("example.com:6309", "/index.md", "Content.", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Should stop quickly with 0 or 1 docs.
	if result.DocumentsCrawled > 1 {
		t.Errorf("DocumentsCrawled = %d, should be 0 or 1 with cancelled context", result.DocumentsCrawled)
	}
}

func TestCrawlerSubdirectories(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [docs/](docs/)\n- [index.md](index.md)\n")
	client.addList("example.com:6309", "/docs/", "- [a.md](a.md)\n- [b.md](b.md)\n")
	client.addDoc("example.com:6309", "/index.md", "# Home", "sha256-0000000000000000000000000000000000000000000000000000000000000000")
	client.addDoc("example.com:6309", "/docs/a.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/docs/b.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	if result.DocumentsCrawled != 3 {
		t.Errorf("DocumentsCrawled = %d, want 3", result.DocumentsCrawled)
	}
	if result.HashesCollected != 3 {
		t.Errorf("HashesCollected = %d, want 3", result.HashesCollected)
	}
}

func TestCrawlerIndexForServer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [a.md](a.md)\n- [b.md](b.md)\n")
	client.addDoc("example.com:6309", "/a.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/b.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	_, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	idx := crawler.IndexForServer("example.com:6309", time.Now())
	if idx == "" {
		t.Fatal("IndexForServer() returned empty string")
	}
	if !contains(idx, "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Error("Index should contain sha256-aaa...")
	}
	if !contains(idx, "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Error("Index should contain sha256-bbb...")
	}
}

func TestCrawlerWithState(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [index.md](index.md)\n")
	client.addDoc("example.com:6309", "/index.md", "# Home", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	state, err := LoadState("/nonexistent/state.json")
	if err != nil {
		t.Fatalf("LoadState() error: %v", err)
	}

	crawler := NewCrawler(cfg, client, state, nil)
	_, err = crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	// Check state was recorded.
	if state.ServerCount() != 1 {
		t.Errorf("ServerCount() = %d, want 1", state.ServerCount())
	}
	if state.URLCount() != 1 {
		t.Errorf("URLCount() = %d, want 1", state.URLCount())
	}

	u := state.GetURL("mark://example.com:6309/index.md")
	if u == nil {
		t.Fatal("GetURL() returned nil")
	}
	if u.ContentHash != "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("ContentHash = %q, want sha256-aaa...", u.ContentHash)
	}
}

func contains(s, substr string) bool {
	return s != "" && substr != "" && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
