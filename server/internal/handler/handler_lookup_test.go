package handler

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// lookupReq builds a LOOKUP request with the given metadata lines.
func lookupReq(scope string, metaLines ...string) string {
	return "LOOKUP " + scope + "\n---\n" + strings.Join(metaLines, "\n") + "\n---\n"
}

// sendLookup runs a request through the handler and parses the response.
func sendLookup(t *testing.T, h *Handler, request string) protocol.Response {
	t.Helper()
	stream := newMockStream(request)
	h.HandleStream(stream)
	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp
}

// lookupHandler returns a handler whose catalog holds the given entries and
// whose store is rooted at an empty temp dir (so scope "/" needs no fixtures).
func lookupHandler(t *testing.T, entries ...*catalog.Entry) *Handler {
	t.Helper()
	cat := catalog.New()
	for _, e := range entries {
		cat.Set(e)
	}
	return &Handler{ContentDir: t.TempDir(), Store: store.New(t.TempDir()), Catalog: cat, Logger: discardLogger}
}

func TestHandleLookupBasic(t *testing.T) {
	h := lookupHandler(t,
		&catalog.Entry{Path: "/docs/auth.md", Tags: []string{"auth", "go"}, Title: "Auth design", Importance: 0.9},
		&catalog.Entry{Path: "/docs/misc.md", Tags: []string{"misc"}, Title: "Unrelated", Importance: 0.5},
	)
	resp := sendLookup(t, h, lookupReq("/", "query: auth"))

	if resp.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want ok", resp.Status)
	}
	if resp.Metadata["matches"] != "1" {
		t.Errorf("matches = %q, want 1", resp.Metadata["matches"])
	}
	if !strings.Contains(resp.Body, "/docs/auth.md") {
		t.Errorf("body missing matched path:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, "/docs/misc.md") {
		t.Errorf("body contains non-matching path:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, "| Path | Importance | Title | Tags |") {
		t.Errorf("body missing table header:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, "0.90") {
		t.Errorf("body missing formatted importance:\n%s", resp.Body)
	}
}

func TestHandleLookupValidation(t *testing.T) {
	h := lookupHandler(t, &catalog.Entry{Path: "/a.md", Tags: []string{"go"}})

	tests := []struct {
		name   string
		req    string
		status string
	}{
		{"missing query", "LOOKUP /\n", protocol.StatusBadRequest},
		{"empty query", lookupReq("/", "query: "), protocol.StatusBadRequest},
		{"one char query", lookupReq("/", "query: a"), protocol.StatusBadRequest},
		{"invalid limit", lookupReq("/", "query: go", "limit: zero"), protocol.StatusBadRequest},
		{"zero limit", lookupReq("/", "query: go", "limit: 0"), protocol.StatusBadRequest},
		{"malformed filter", lookupReq("/", "query: go", "filter: project"), protocol.StatusBadRequest},
		{"bad filter date", lookupReq("/", "query: go", "filter: modified-after=nope"), protocol.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := sendLookup(t, h, tt.req)
			if resp.Status != tt.status {
				t.Errorf("status = %q, want %q", resp.Status, tt.status)
			}
		})
	}
}

func TestHandleLookupEmptyResults(t *testing.T) {
	h := lookupHandler(t, &catalog.Entry{Path: "/a.md", Tags: []string{"go"}})
	resp := sendLookup(t, h, lookupReq("/", "query: kubernetes"))

	if resp.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want ok", resp.Status)
	}
	if resp.Metadata["matches"] != "0" {
		t.Errorf("matches = %q, want 0", resp.Metadata["matches"])
	}
	// Header-only table, no data rows.
	if strings.Contains(resp.Body, "/a.md") {
		t.Errorf("empty result body should not list documents:\n%s", resp.Body)
	}
}

func TestHandleLookupScopeNotFound(t *testing.T) {
	h := lookupHandler(t, &catalog.Entry{Path: "/a.md", Tags: []string{"go"}})
	resp := sendLookup(t, h, lookupReq("/nope/", "query: go"))
	if resp.Status != protocol.StatusNotFound {
		t.Errorf("status = %q, want not-found", resp.Status)
	}
}

func TestHandleLookupNotConfigured(t *testing.T) {
	h := &Handler{ContentDir: t.TempDir(), Store: store.New(t.TempDir()), Logger: discardLogger}
	resp := sendLookup(t, h, lookupReq("/", "query: go"))
	if resp.Status != protocol.StatusServerError {
		t.Errorf("status = %q, want server-error", resp.Status)
	}
}

func TestHandleLookupLimit(t *testing.T) {
	h := lookupHandler(t,
		&catalog.Entry{Path: "/a.md", Tags: []string{"go"}, Importance: 0.9},
		&catalog.Entry{Path: "/b.md", Tags: []string{"go"}, Importance: 0.8},
		&catalog.Entry{Path: "/c.md", Tags: []string{"go"}, Importance: 0.7},
	)
	resp := sendLookup(t, h, lookupReq("/", "query: go", "limit: 2"))
	if resp.Metadata["matches"] != "2" {
		t.Errorf("matches = %q, want 2", resp.Metadata["matches"])
	}
}

// TestHandleLookupReadAuthFiltering is the leakage test: protected documents
// the requester cannot read must never appear in results.
func TestHandleLookupReadAuthFiltering(t *testing.T) {
	const readSecret = "read-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(readSecret): {
			Paths:      []string{"/private/*"},
			Operations: []string{"read"},
		},
	})
	h := lookupHandler(t,
		&catalog.Entry{Path: "/public/doc.md", Tags: []string{"auth"}, Title: "Public"},
		&catalog.Entry{Path: "/private/secret.md", Tags: []string{"auth"}, Title: "Secret"},
	)
	h.GetTokenStore = func() *auth.TokenStore { return ts }

	t.Run("without token omits protected doc", func(t *testing.T) {
		resp := sendLookup(t, h, lookupReq("/", "query: auth"))
		if resp.Metadata["matches"] != "1" {
			t.Errorf("matches = %q, want 1 (protected doc must be hidden)", resp.Metadata["matches"])
		}
		if strings.Contains(resp.Body, "/private/secret.md") || strings.Contains(resp.Body, "Secret") {
			t.Errorf("protected document leaked into results:\n%s", resp.Body)
		}
		if !strings.Contains(resp.Body, "/public/doc.md") {
			t.Errorf("public document missing:\n%s", resp.Body)
		}
	})

	t.Run("with token reveals protected doc", func(t *testing.T) {
		resp := sendLookup(t, h, lookupReq("/", "query: auth", "auth: "+readSecret))
		if resp.Metadata["matches"] != "2" {
			t.Errorf("matches = %q, want 2", resp.Metadata["matches"])
		}
		if !strings.Contains(resp.Body, "/private/secret.md") {
			t.Errorf("authorized requester should see protected doc:\n%s", resp.Body)
		}
	})
}

// TestHandleLookupCatalogUpdates verifies PUBLISH/ARCHIVE keep the catalog in
// sync through the handler's write paths.
func TestHandleLookupCatalogUpdates(t *testing.T) {
	const secret = "write-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})
	dir := t.TempDir()
	h := &Handler{
		ContentDir:    dir,
		Store:         store.New(dir),
		Catalog:       catalog.New(),
		Logger:        discardLogger,
		GetTokenStore: func() *auth.TokenStore { return ts },
	}

	// Not present before publishing.
	if resp := sendLookup(t, h, lookupReq("/", "query: middleware")); resp.Metadata["matches"] != "0" {
		t.Fatalf("pre-publish matches = %q, want 0", resp.Metadata["matches"])
	}

	// PUBLISH adds it to the catalog.
	pub := newMockStream("PUBLISH /auth.md\n---\nauth: " + secret + "\ntags: middleware,go\n---\n# Auth Middleware\n")
	h.HandleStream(pub)
	if resp, _ := protocol.ParseResponse(&pub.output); resp.Status != protocol.StatusCreated {
		t.Fatalf("publish status = %q, want created", resp.Status)
	}
	resp := sendLookup(t, h, lookupReq("/", "query: middleware"))
	if resp.Metadata["matches"] != "1" {
		t.Fatalf("post-publish matches = %q, want 1", resp.Metadata["matches"])
	}
	if !strings.Contains(resp.Body, "/auth.md") {
		t.Errorf("published doc missing from lookup:\n%s", resp.Body)
	}
	// Title is derived from the body's H1 (no declared title). This confirms the
	// catalog received the clean body from WriteVersion, not store-frontmatter
	// bytes — a corrupted body would not yield "Auth Middleware" here.
	if !strings.Contains(resp.Body, "Auth Middleware") {
		t.Errorf("title not derived from body H1 (catalog body may be corrupted):\n%s", resp.Body)
	}

	// ARCHIVE removes it from the catalog.
	arch := newMockStream("ARCHIVE /auth.md\n---\nauth: " + secret + "\n---\n")
	h.HandleStream(arch)
	if resp, _ := protocol.ParseResponse(&arch.output); resp.Status != protocol.StatusOK {
		t.Fatalf("archive status = %q, want ok", resp.Status)
	}
	if resp := sendLookup(t, h, lookupReq("/", "query: middleware")); resp.Metadata["matches"] != "0" {
		t.Errorf("post-archive matches = %q, want 0", resp.Metadata["matches"])
	}
}
