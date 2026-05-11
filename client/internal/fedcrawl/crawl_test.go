package fedcrawl

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
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
}

func newMockClient() *mockClient {
	return &mockClient{
		pages:           make(map[string]mockPage),
		lists:           make(map[string]mockPage),
		publishStatuses: make(map[string]string),
	}
}

func (m *mockClient) Publish(host, path, body, token string, expectedVersion int, _ map[string]string) (fetch.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "PUBLISH "+host+path)
	m.publishes = append(m.publishes, publishCall{
		Host:            host,
		Path:            path,
		Body:            body,
		Token:           token,
		ExpectedVersion: expectedVersion,
	})
	status := protocol.StatusCreated
	if s, ok := m.publishStatuses[host+path]; ok {
		status = s
	}
	return fetch.Result{Response: protocol.Response{Status: status}}, nil
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
