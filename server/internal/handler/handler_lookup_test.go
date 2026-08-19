package handler

import (
	"io"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
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

// seedDoc is one document to publish before a LOOKUP test: path and the
// publisher metadata the catalog derives its entry from.
type seedDoc struct {
	path string
	meta map[string]string
}

// lookupHandler returns a handler over b with docs published through the
// store and registered in the catalog the way the handler's PUBLISH path
// does, so the test means the same thing on every backend.
func lookupHandler(t *testing.T, b backend, docs ...seedDoc) *Handler {
	t.Helper()
	for _, d := range docs {
		doc, err := b.Store.WriteVersion(d.path, -1, []byte("# "+d.path+"\n"), d.meta)
		if err != nil {
			t.Fatalf("seed %s: %v", d.path, err)
		}
		b.Catalog.Put(d.path, doc.Metadata, doc.Content, doc.Modified)
	}
	return newHandler(b, nil)
}

func TestHandleLookupBasic(t *testing.T) { forEachBackend(t, testHandleLookupBasic) }

func testHandleLookupBasic(t *testing.T, newBackend backendFactory) {
	h := lookupHandler(t, newBackend(t),
		seedDoc{"/docs/auth.md", map[string]string{"tags": "auth, go", "title": "Auth design", "importance": "0.9"}},
		seedDoc{"/docs/misc.md", map[string]string{"tags": "misc", "title": "Unrelated", "importance": "0.5"}},
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

func TestHandleLookupValidation(t *testing.T) { forEachBackend(t, testHandleLookupValidation) }

func testHandleLookupValidation(t *testing.T, newBackend backendFactory) {
	h := lookupHandler(t, newBackend(t), seedDoc{"/a.md", map[string]string{"tags": "go"}})

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

func TestHandleLookupEmptyResults(t *testing.T) { forEachBackend(t, testHandleLookupEmptyResults) }

func testHandleLookupEmptyResults(t *testing.T, newBackend backendFactory) {
	h := lookupHandler(t, newBackend(t), seedDoc{"/a.md", map[string]string{"tags": "go"}})
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

func TestHandleLookupScopeNotFound(t *testing.T) { forEachBackend(t, testHandleLookupScopeNotFound) }

func testHandleLookupScopeNotFound(t *testing.T, newBackend backendFactory) {
	h := lookupHandler(t, newBackend(t), seedDoc{"/a.md", map[string]string{"tags": "go"}})
	resp := sendLookup(t, h, lookupReq("/nope/", "query: go"))
	if resp.Status != protocol.StatusNotFound {
		t.Errorf("status = %q, want not-found", resp.Status)
	}
}

func TestHandleLookupNotConfigured(t *testing.T) {
	h := &Handler{Store: fileBackend(t).Store, Logger: discardLogger}
	resp := sendLookup(t, h, lookupReq("/", "query: go"))
	if resp.Status != protocol.StatusServerError {
		t.Errorf("status = %q, want server-error", resp.Status)
	}
}

func TestHandleLookupLimit(t *testing.T) { forEachBackend(t, testHandleLookupLimit) }

func testHandleLookupLimit(t *testing.T, newBackend backendFactory) {
	h := lookupHandler(t, newBackend(t),
		seedDoc{"/a.md", map[string]string{"tags": "go", "importance": "0.9"}},
		seedDoc{"/b.md", map[string]string{"tags": "go", "importance": "0.8"}},
		seedDoc{"/c.md", map[string]string{"tags": "go", "importance": "0.7"}},
	)
	resp := sendLookup(t, h, lookupReq("/", "query: go", "limit: 2"))
	if resp.Metadata["matches"] != "2" {
		t.Errorf("matches = %q, want 2", resp.Metadata["matches"])
	}
}

// TestHandleLookupReadAuthFiltering is the leakage test: protected documents
// the requester cannot read must never appear in results.
func TestHandleLookupReadAuthFiltering(t *testing.T) {
	forEachBackend(t, testHandleLookupReadAuthFiltering)
}

func testHandleLookupReadAuthFiltering(t *testing.T, newBackend backendFactory) {
	const readSecret = "read-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(readSecret): {
			Paths:      []string{"/private/*"},
			Operations: []string{"read"},
		},
	})
	h := lookupHandler(t, newBackend(t),
		seedDoc{"/public/doc.md", map[string]string{"tags": "auth", "title": "Public"}},
		seedDoc{"/private/secret.md", map[string]string{"tags": "auth", "title": "Secret"}},
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

// mustStatus parses a handler response, failing the test on a malformed one so
// a format regression is never mistaken for a status mismatch.
func mustStatus(t *testing.T, out io.Reader) string {
	t.Helper()
	resp, err := protocol.ParseResponse(out)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp.Status
}

// catalogHandler returns a handler with a live catalog and a publish token,
// plus the token's secret, for tests that drive the write paths.
func catalogHandler(t *testing.T, b backend) (h *Handler, secret string) {
	t.Helper()
	secret = "write-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		},
	})
	return &Handler{
		Store:         b.Store,
		Catalog:       b.Catalog,
		Logger:        discardLogger,
		GetTokenStore: func() *auth.TokenStore { return ts },
	}, secret
}

// TestHandleLookupCatalogUpdates verifies PUBLISH/ARCHIVE keep the catalog in
// sync through the handler's write paths.
func TestHandleLookupCatalogUpdates(t *testing.T) { forEachBackend(t, testHandleLookupCatalogUpdates) }

func testHandleLookupCatalogUpdates(t *testing.T, newBackend backendFactory) {
	h, secret := catalogHandler(t, newBackend(t))

	// Not present before publishing.
	if resp := sendLookup(t, h, lookupReq("/", "query: middleware")); resp.Metadata["matches"] != "0" {
		t.Fatalf("pre-publish matches = %q, want 0", resp.Metadata["matches"])
	}

	// PUBLISH adds it to the catalog.
	pub := newMockStream("PUBLISH /auth.md\n---\nauth: " + secret + "\ntags: middleware,go\n---\n# Auth Middleware\n")
	h.HandleStream(pub)
	if status := mustStatus(t, &pub.output); status != protocol.StatusCreated {
		t.Fatalf("publish status = %q, want created", status)
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
	if status := mustStatus(t, &arch.output); status != protocol.StatusOK {
		t.Fatalf("archive status = %q, want ok", status)
	}
	if resp := sendLookup(t, h, lookupReq("/", "query: middleware")); resp.Metadata["matches"] != "0" {
		t.Errorf("post-archive matches = %q, want 0", resp.Metadata["matches"])
	}
}

// TestHandleLookupCatalogSurvivesAppend guards the defect where an APPEND
// re-indexed the catalog from the request's metadata alone, dropping the
// document's tags and importance so it fell out of LOOKUP entirely.
func TestHandleLookupCatalogSurvivesAppend(t *testing.T) {
	forEachBackend(t, testHandleLookupCatalogSurvivesAppend)
}

func testHandleLookupCatalogSurvivesAppend(t *testing.T, newBackend backendFactory) {
	h, secret := catalogHandler(t, newBackend(t))

	// "rbac" appears only in the tags, never in the H1: a title-matching query
	// would pass through the catalog's H1 fallback even with every tag gone.
	pub := newMockStream("PUBLISH /auth.md\n---\nauth: " + secret + "\ntags: rbac,go\nimportance: 0.9\n---\n# Auth Middleware\n")
	h.HandleStream(pub)
	if status := mustStatus(t, &pub.output); status != protocol.StatusCreated {
		t.Fatalf("publish status = %q, want created", status)
	}

	app := newMockStream("APPEND /auth.md\n---\nauth: " + secret + "\nexpected-version: 1\nagent: claude\n---\n\n## More\n")
	h.HandleStream(app)
	if status := mustStatus(t, &app.output); status != protocol.StatusCreated {
		t.Fatalf("append status = %q, want created", status)
	}

	resp := sendLookup(t, h, lookupReq("/", "query: rbac"))
	if resp.Metadata["matches"] != "1" {
		t.Fatalf("post-append matches = %q, want 1 (tags dropped by append)", resp.Metadata["matches"])
	}
	if !strings.Contains(resp.Body, "/auth.md") {
		t.Errorf("appended doc missing from lookup:\n%s", resp.Body)
	}

	fetch := newMockStream("FETCH /auth.md\n")
	h.HandleStream(fetch)
	got, err := protocol.ParseResponse(&fetch.output)
	if err != nil {
		t.Fatalf("parse fetch: %v", err)
	}
	if got.Metadata["importance"] != "0.9" {
		t.Errorf("importance = %q, want 0.9 carried through the append", got.Metadata["importance"])
	}
	if got.Metadata["agent"] != "claude" {
		t.Errorf("agent = %q, want the append's own key applied", got.Metadata["agent"])
	}
}

func TestHandleLookupMatchAll(t *testing.T) { forEachBackend(t, testHandleLookupMatchAll) }

func testHandleLookupMatchAll(t *testing.T, newBackend backendFactory) {
	// "*" passes the length validation and returns the whole catalog under
	// the scope in importance order — the universe-browser query.
	h := lookupHandler(t, newBackend(t),
		seedDoc{"/hub.md", map[string]string{"tags": "hub", "title": "Hub", "importance": "0.9"}},
		seedDoc{"/note.md", map[string]string{"tags": "misc", "title": "Note", "importance": "0.3"}},
	)
	resp := sendLookup(t, h, lookupReq("/", "query: '*'"))
	if resp.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want ok (match-all must pass validation)", resp.Status)
	}
	if resp.Metadata["matches"] != "2" {
		t.Errorf("matches = %q, want 2", resp.Metadata["matches"])
	}
	if hub, note := strings.Index(resp.Body, "/hub.md"), strings.Index(resp.Body, "/note.md"); hub == -1 || note == -1 || hub > note {
		t.Errorf("want both paths, importance order:\n%s", resp.Body)
	}
}
