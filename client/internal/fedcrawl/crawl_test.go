package fedcrawl

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// mockClient implements FetchClient and PublishClient for testing.
type mockClient struct {
	mu    sync.Mutex
	pages map[string]mockPage // keyed by "host/path"
	lists map[string]mockPage // keyed by "host/path" for LIST results
	calls []string

	// publishStatuses overrides the default created-on-publish behavior.
	// Keyed by host+path so multi-hub tests can model per-hub status differences
	// when several hubs share the same target path (e.g. /index.md).
	publishStatuses map[string]string
	publishes       []publishCall
}

type mockPage struct {
	status   string
	body     string
	metadata map[string]string
}

type publishCall struct {
	Host            string
	Path            string
	Body            string
	Token           string
	ExpectedVersion int
	Meta            map[string]string
}

func newMockClient() *mockClient {
	return &mockClient{
		pages:           make(map[string]mockPage),
		lists:           make(map[string]mockPage),
		publishStatuses: make(map[string]string),
	}
}

func (m *mockClient) Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "PUBLISH "+host+path)
	m.publishes = append(m.publishes, publishCall{
		Host:            host,
		Path:            path,
		Body:            body,
		Token:           token,
		ExpectedVersion: expectedVersion,
		Meta:            meta,
	})
	status := protocol.StatusCreated
	if s, ok := m.publishStatuses[host+path]; ok {
		status = s
	}
	return fetch.Result{Response: protocol.Response{Status: status}}, nil
}

func (m *mockClient) addDoc(host, path, body, hash string) {
	m.addDocWithMeta(host, path, body, hash, nil)
}

func (m *mockClient) addDocWithMeta(host, path, body, hash string, extra map[string]string) {
	metadata := map[string]string{
		"etag":         "etag-" + path,
		"content-hash": hash,
	}
	maps.Copy(metadata, extra)
	m.pages[host+path] = mockPage{
		status:   protocol.StatusOK,
		body:     body,
		metadata: metadata,
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
	t.Run("single_server", func(t *testing.T) {
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
	})

	t.Run("cross_server", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://server1.com"}
		cfg.Crawl.MaxDocuments = 1
		cfg.Crawl.Workers = 2 // Multiple workers to test concurrent cap

		client := newMockClient()
		client.addList("server1.com:6309", "/", "- [a.md](a.md)\n")
		client.addDoc("server1.com:6309", "/a.md", "Link to [server2](mark://server2.com/b.md).", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		client.addList("server2.com:6309", "/", "- [b.md](b.md)\n")
		client.addDoc("server2.com:6309", "/b.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		crawler := NewCrawler(cfg, client, nil, nil)
		result, err := crawler.Run(context.Background())
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		if result.DocumentsCrawled > 1 {
			t.Errorf("DocumentsCrawled = %d, should be capped at 1 across servers", result.DocumentsCrawled)
		}
	})
}

func TestCrawlerMaxServers(t *testing.T) {
	t.Run("discovery_cap", func(t *testing.T) {
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
	})

	t.Run("seed_cap", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://server1.com", "mark://server2.com"}
		cfg.Crawl.MaxServers = 1

		client := newMockClient()
		client.addList("server1.com:6309", "/", "- [index.md](index.md)\n")
		client.addDoc("server1.com:6309", "/index.md", "Content.", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		client.addList("server2.com:6309", "/", "- [index.md](index.md)\n")
		client.addDoc("server2.com:6309", "/index.md", "Content.", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		crawler := NewCrawler(cfg, client, nil, nil)
		result, err := crawler.Run(context.Background())
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		if result.ServersDiscovered > 1 {
			t.Errorf("ServersDiscovered = %d, should be capped at 1 even with multiple seeds", result.ServersDiscovered)
		}
	})
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

func TestPublishIndex(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantErr    bool
		wantErrSub string
	}{
		{name: "created status accepted (first publish)", status: protocol.StatusCreated},
		{name: "ok status accepted (re-publish or no-op)", status: protocol.StatusOK},
		{name: "conflict status surfaces error", status: protocol.StatusConflict, wantErr: true, wantErrSub: "conflict"},
		{name: "not-found status surfaces error", status: protocol.StatusNotFound, wantErr: true, wantErrSub: "not-found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newMockClient()
			client.publishStatuses["hub.example.com:6309/index.md"] = tt.status

			crawler := NewCrawler(DefaultConfig(), client, nil, nil)
			err := crawler.publishIndex(context.Background(), client, "hub.example.com:6309", "/index.md", "# Index\n", "tok")

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantErrSub != "" && !containsMiddle(err.Error(), tt.wantErrSub) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(client.publishes) != 1 {
				t.Fatalf("expected 1 publish, got %d", len(client.publishes))
			}
			call := client.publishes[0]
			if call.ExpectedVersion != -1 {
				t.Errorf("ExpectedVersion = %d, want -1 (no-check) for idempotent re-publish", call.ExpectedVersion)
			}
			if call.Path != "/index.md" {
				t.Errorf("Path = %q, want /index.md", call.Path)
			}
			if call.Token != "tok" {
				t.Errorf("Token = %q, want tok", call.Token)
			}
		})
	}
}

func TestPublishIndexRePublish(t *testing.T) {
	// Two consecutive publishes to the same path: first returns created,
	// second returns ok. Both must succeed (the daemon-mode case).
	client := newMockClient()
	crawler := NewCrawler(DefaultConfig(), client, nil, nil)

	if err := crawler.publishIndex(context.Background(), client, "hub.example.com:6309", "/index.md", "# v1\n", "tok"); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	client.mu.Lock()
	client.publishStatuses["hub.example.com:6309/index.md"] = protocol.StatusOK
	client.mu.Unlock()

	if err := crawler.publishIndex(context.Background(), client, "hub.example.com:6309", "/index.md", "# v2\n", "tok"); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	if len(client.publishes) != 2 {
		t.Fatalf("expected 2 publishes, got %d", len(client.publishes))
	}
	for i, call := range client.publishes {
		if call.ExpectedVersion != -1 {
			t.Errorf("publishes[%d].ExpectedVersion = %d, want -1 (no-check) for idempotent re-publish", i, call.ExpectedVersion)
		}
	}
}

func TestPublishToHubs(t *testing.T) {
	t.Run("no hubs returns zero publishes", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		// no hubs configured
		client := newMockClient()
		crawler := NewCrawler(cfg, client, nil, nil)
		count, err := crawler.PublishToHubs(context.Background(), client, false)
		if err != nil {
			t.Fatalf("PublishToHubs() error: %v", err)
		}
		if count != 0 {
			t.Errorf("count = %d, want 0", count)
		}
		if len(client.publishes) != 0 {
			t.Errorf("expected 0 publishes, got %d", len(client.publishes))
		}
	})

	t.Run("aggregated mode publishes single /index.md", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://example.com"}
		cfg.Hubs = []string{"mark://hub.example.com"}

		client := newMockClient()
		client.addList("example.com:6309", "/", "- [a.md](a.md)\n")
		client.addDoc("example.com:6309", "/a.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

		crawler := NewCrawler(cfg, client, nil, nil)
		if _, err := crawler.Run(context.Background()); err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		count, err := crawler.PublishToHubs(context.Background(), client, false)
		if err != nil {
			t.Fatalf("PublishToHubs() error: %v", err)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1", count)
		}
		if len(client.publishes) != 1 {
			t.Fatalf("expected 1 publish, got %d", len(client.publishes))
		}
		if client.publishes[0].Path != "/index.md" {
			t.Errorf("publish path = %q, want /index.md", client.publishes[0].Path)
		}
	})

	t.Run("per-server mode publishes one index per discovered server", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://a.example.com", "mark://b.example.com"}
		cfg.Hubs = []string{"mark://hub.example.com"}

		client := newMockClient()
		client.addList("a.example.com:6309", "/", "- [doc.md](doc.md)\n")
		client.addDoc("a.example.com:6309", "/doc.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		client.addList("b.example.com:6309", "/", "- [doc.md](doc.md)\n")
		client.addDoc("b.example.com:6309", "/doc.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

		crawler := NewCrawler(cfg, client, nil, nil)
		if _, err := crawler.Run(context.Background()); err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		count, err := crawler.PublishToHubs(context.Background(), client, true)
		if err != nil {
			t.Fatalf("PublishToHubs() error: %v", err)
		}
		if count != 2 {
			t.Errorf("count = %d, want 2", count)
		}
		if len(client.publishes) != 2 {
			t.Fatalf("expected 2 publishes, got %d", len(client.publishes))
		}
		for i, call := range client.publishes {
			if call.ExpectedVersion != -1 {
				t.Errorf("publishes[%d].ExpectedVersion = %d, want -1 (no-check)", i, call.ExpectedVersion)
			}
		}
	})
}

func TestCrawlerGraphExportAndPublish(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://a.example.com"}
	cfg.Hubs = []string{"mark://hub.example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("a.example.com:6309", "/", "- [index.md](index.md)\n- [notes.md](notes.md)\n")
	// index.md links to a sibling and to a doc on another server.
	client.addDoc("a.example.com:6309", "/index.md",
		"# A\n[notes](notes.md) and [cross](mark://b.example.com:6309/page.md)",
		"sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("a.example.com:6309", "/notes.md", "# Notes",
		"sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	client.addList("b.example.com:6309", "/", "- [page.md](page.md)\n")
	client.addDoc("b.example.com:6309", "/page.md", "# B",
		"sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	if _, err := crawler.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	exp := crawler.GraphExport()
	for _, want := range []string{
		"# Document Graph",
		"## Edges",
		// intra-server edge (relative link resolved)
		"mark://a.example.com/index.md | mark://a.example.com/notes.md",
		// cross-server edge — the connection that makes a portal/edge on the floor
		"mark://a.example.com/index.md | mark://b.example.com/page.md",
	} {
		if !contains(exp, want) {
			t.Errorf("graph export missing %q\n---\n%s", want, exp)
		}
	}

	n, err := crawler.PublishGraphToHubs(context.Background(), client)
	if err != nil {
		t.Fatalf("PublishGraphToHubs() error: %v", err)
	}
	if n != 1 {
		t.Errorf("published = %d, want 1", n)
	}
	var graphPublished bool
	for _, call := range client.publishes {
		if call.Host == "hub.example.com:6309" && call.Path == "/graph.md" {
			graphPublished = true
		}
	}
	if !graphPublished {
		t.Errorf("no /graph.md publish to the hub: %+v", client.publishes)
	}
}

func TestCrawlerGraphExportFiltersLoopbackAndNormalizesPorts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://a.example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("a.example.com:6309", "/", "- [index.md](index.md)\n")
	// The doc links to IPv4 loopback, IPv6 loopback, localhost (dev artifacts)
	// and a port-less external world.
	client.addDoc("a.example.com:6309", "/index.md",
		"# A\n[loop](mark://127.0.0.1:6401/x.md) [loop6](mark://[::1]/q.md) [local](mark://localhost/y.md) [ext](mark://ext.example.com/page.md)",
		"sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	crawler := NewCrawler(cfg, client, nil, nil)
	if _, err := crawler.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	exp := crawler.GraphExport()

	// loopback (v4 + v6) / localhost targets never enter the durable graph.
	for _, bad := range []string{"127.0.0.1", "::1", "localhost"} {
		if containsMiddle(exp, bad) {
			t.Errorf("graph export must not contain %q\n---\n%s", bad, exp)
		}
	}
	// the external target is kept, normalized to identity form (ADR 0005).
	if !containsMiddle(exp, "mark://a.example.com/index.md | mark://ext.example.com/page.md") {
		t.Errorf("graph export missing normalized external edge\n---\n%s", exp)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:6401":        true,  // IPv4 loopback + port
		"localhost":             true,  // bare localhost
		"localhost:6309":        true,  // localhost + port
		"0.0.0.0:6309":          true,  // unspecified
		"[::1]:6309":            true,  // bracketed IPv6 loopback + port
		"::1":                   true,  // bare IPv6 loopback
		"::1:6309":              true,  // unbracketed IPv6 loopback + port
		"soul.demarkus.io:6309": false, // real external world
		"10.0.0.5:6309":         false, // private IP — a real LAN/cluster world, kept
		"2001:db8::5":           false, // routable IPv6
	}
	for in, want := range cases {
		if got := isLoopbackHost(in); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", in, got, want)
		}
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

func TestPublishRetentionMeta(t *testing.T) {
	setup := func(retention int) (*Crawler, *mockClient) {
		cfg := DefaultConfig()
		cfg.Seeds = []string{"mark://a.example.com"}
		cfg.Hubs = []string{"mark://hub.example.com"}
		cfg.Publish.Retention = retention
		client := newMockClient()
		client.addList("a.example.com:6309", "/", "- [index.md](index.md)\n")
		client.addDoc("a.example.com:6309", "/index.md", "# A",
			"sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		crawler := NewCrawler(cfg, client, nil, nil)
		if _, err := crawler.Run(context.Background()); err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		return crawler, client
	}

	t.Run("default caps published artifacts at 20", func(t *testing.T) {
		if got := DefaultConfig().Publish.Retention; got != 20 {
			t.Fatalf("default publish.retention = %d, want 20", got)
		}
		crawler, client := setup(DefaultConfig().Publish.Retention)
		if _, err := crawler.PublishGraphToHubs(context.Background(), client); err != nil {
			t.Fatalf("PublishGraphToHubs() error: %v", err)
		}
		if _, err := crawler.PublishToHubs(context.Background(), client, false); err != nil {
			t.Fatalf("PublishToHubs() error: %v", err)
		}
		if len(client.publishes) == 0 {
			t.Fatal("expected publishes")
		}
		for _, call := range client.publishes {
			if call.Meta["retention"] != "20" {
				t.Errorf("publish %s%s meta.retention = %q, want %q", call.Host, call.Path, call.Meta["retention"], "20")
			}
			if call.Meta["agent"] != "demarkus-agent" {
				t.Errorf("publish %s%s lost agent meta: %v", call.Host, call.Path, call.Meta)
			}
		}
	})

	t.Run("zero keeps every version", func(t *testing.T) {
		crawler, client := setup(0)
		if _, err := crawler.PublishGraphToHubs(context.Background(), client); err != nil {
			t.Fatalf("PublishGraphToHubs() error: %v", err)
		}
		if len(client.publishes) == 0 {
			t.Fatal("expected publishes")
		}
		for _, call := range client.publishes {
			if _, ok := call.Meta["retention"]; ok {
				t.Errorf("publish %s%s should carry no retention meta: %v", call.Host, call.Path, call.Meta)
			}
		}
	})
}

func TestConfigValidate_PublishRetention(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://a.example.com"}
	cfg.Publish.Retention = -1
	if err := cfg.Validate(); err == nil {
		t.Error("negative publish.retention should fail validation")
	}
	cfg.Publish.Retention = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero publish.retention should validate: %v", err)
	}
}

func TestCrawlerRecordsTypedAndProvenancedEdges(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://a.example.com"}
	cfg.Crawl.MaxDocuments = 100

	client := newMockClient()
	client.addList("a.example.com:6309", "/", "- [index.md](index.md)\n- [notes.md](notes.md)\n")
	client.addDocWithMeta("a.example.com:6309", "/index.md",
		"# A\n\n## Guide\n\nSee [the notes](notes.md).",
		"sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		map[string]string{"rel-supersedes": "/old.md, mark://127.0.0.1:6309/dev.md"})
	client.addDoc("a.example.com:6309", "/notes.md", "# Notes",
		"sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	crawler := NewCrawler(cfg, client, nil, nil)
	if _, err := crawler.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	exp := crawler.GraphExport()
	for _, want := range []string{
		// body link with label and source anchor
		"| mark://a.example.com/index.md | mark://a.example.com/notes.md |  | the notes | guide | 1 |",
		// typed relation from rel-supersedes metadata, resolved against the doc URL
		"| mark://a.example.com/index.md | mark://a.example.com/old.md | supersedes |  |  | 1 |",
	} {
		if !contains(exp, want) {
			t.Errorf("graph export missing %q\n---\n%s", want, exp)
		}
	}
	// The loopback rel target must be filtered like a loopback body link.
	if contains(exp, "127.0.0.1") {
		t.Errorf("loopback rel target leaked into export\n---\n%s", exp)
	}
}

// TestRecordEdgesTitlePrecedence: declared metadata title wins; a doc with
// only an H1 falls back to it (blank titles propagate to every hub-graph
// consumer otherwise).
func TestRecordEdgesTitlePrecedence(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Politeness.RequestDelay = 0

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [declared.md](declared.md)\n- [heading.md](heading.md)\n- [bare.md](bare.md)\n")
	client.addDocWithMeta("example.com:6309", "/declared.md", "# Heading Title\n", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", map[string]string{"title": "Declared Title"})
	client.addDocWithMeta("example.com:6309", "/heading.md", "# Heading Only\n", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil)
	client.addDocWithMeta("example.com:6309", "/bare.md", "no heading at all\n", "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", nil)

	c := NewCrawler(cfg, client, nil, nil)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := map[string]string{
		"mark://example.com:6309/declared.md": "Declared Title",
		"mark://example.com:6309/heading.md":  "Heading Only",
		"mark://example.com:6309/bare.md":     "",
	}
	for _, n := range c.graph.AllNodes() {
		expect, ok := want[n.URL]
		if !ok {
			continue
		}
		if n.Title != expect {
			t.Errorf("title of %s = %q, want %q", n.URL, n.Title, expect)
		}
		delete(want, n.URL)
	}
	if len(want) != 0 {
		t.Errorf("nodes not crawled: %v", want)
	}
}

func TestCrawlerListingCycle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDocuments = 100
	cfg.Crawl.MaxDepth = 3
	cfg.Politeness.RequestDelay = 0

	client := newMockClient()
	// A self-referencing listing served at EVERY depth mints ever-deeper
	// distinct paths; termination is demonstrated by MaxDepth, not by the
	// visited set or a missing mock response.
	client.addList("example.com:6309", "/", "- [a.md](a.md)\n- [loop/](loop/)\n")
	dir := "/loop/"
	for range 8 {
		client.addList("example.com:6309", dir, "- [loop/](loop/)\n- [b.md](b.md)\n")
		dir += "loop/"
	}
	client.addDoc("example.com:6309", "/a.md", "# A", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/loop/b.md", "# B", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// a.md + /loop/b.md; deeper b.md paths have no documents behind them.
	if result.DocumentsCrawled != 2 {
		t.Errorf("DocumentsCrawled = %d, want 2", result.DocumentsCrawled)
	}
}

func TestCrawlerMaxDepth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seeds = []string{"mark://example.com"}
	cfg.Crawl.MaxDepth = 1
	cfg.Crawl.MaxDocuments = 100
	cfg.Politeness.RequestDelay = 0

	client := newMockClient()
	client.addList("example.com:6309", "/", "- [top.md](top.md)\n- [d1/](d1/)\n")
	client.addList("example.com:6309", "/d1/", "- [mid.md](mid.md)\n- [d2/](d2/)\n")
	client.addList("example.com:6309", "/d1/d2/", "- [deep.md](deep.md)\n")
	client.addDoc("example.com:6309", "/top.md", "# Top", "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	client.addDoc("example.com:6309", "/d1/mid.md", "# Mid", "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	client.addDoc("example.com:6309", "/d1/d2/deep.md", "# Deep", "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	crawler := NewCrawler(cfg, client, nil, nil)
	result, err := crawler.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// depth 0 = /, depth 1 = /d1/; /d1/d2/ is depth 2 and must not be listed.
	if result.DocumentsCrawled != 2 {
		t.Errorf("DocumentsCrawled = %d, want 2 (deep.md is past MaxDepth)", result.DocumentsCrawled)
	}
}
