package storetest

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// RunLookupHandlerConformance exercises the wire contract of LOOKUP's match
// key through the handler: the echo, the fifth column, read-auth filtering
// per row, and the byte-identical catalog mode.
func RunLookupHandlerConformance(t *testing.T, factory LookupFactory) {
	subtests := []lookupSubtest{
		{"BodyEchoAndColumns", testHandlerBodyEchoAndColumns},
		{"CatalogByteIdentical", testHandlerCatalogByteIdentical},
		{"BodyUnknownModeBadRequest", testHandlerBodyUnknownMode},
		{"BodyReadAuthFiltering", testHandlerBodyReadAuth},
	}
	for _, st := range subtests {
		t.Run(st.name, func(t *testing.T) {
			st.fn(t, factory(t))
		})
	}
}

const readSecret = "storetest-read-token"

// newReadAuthHandler is NewHandler plus a read token for /private/*, which
// makes that subtree read-protected.
func newReadAuthHandler(b LookupBackend) *handler.Handler {
	return newHandlerWithTokens(b, map[string]auth.Token{
		protocol.HashToken(readSecret): {Paths: []string{"/private/*"}, Operations: []string{"read"}},
	})
}

func publishDoc(t *testing.T, h *handler.Handler, path, body string, meta map[string]string) {
	t.Helper()
	if resp := Send(t, h, request(protocol.VerbPublish, path, meta, body)); resp.Status != protocol.StatusCreated {
		t.Fatalf("publish %s: status %q (%s)", path, resp.Status, resp.Body)
	}
}

func lookup(t *testing.T, h *handler.Handler, scope string, meta map[string]string) protocol.Response {
	t.Helper()
	return Send(t, h, request(protocol.VerbLookup, scope, meta, ""))
}

func testHandlerBodyEchoAndColumns(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/debugging.md", debuggingDoc, map[string]string{"tags": "debugging,gotchas", "importance": "0.7"})
	publishDoc(t, h, "/plain.md", "a headingless note about hairpin nat\n", map[string]string{"title": "Plain"})

	resp := lookup(t, h, "/", map[string]string{"query": "sysctl", "match": "body"})
	if resp.Status != protocol.StatusOK || resp.Metadata["match"] != "body" || resp.Metadata["matches"] != "1" {
		t.Fatalf("status %q match %q matches %q, want ok body 1 (%s)", resp.Status, resp.Metadata["match"], resp.Metadata["matches"], resp.Body)
	}
	for _, want := range []string{
		"| Path | Importance | Title | Tags | Snippet |",
		"| /debugging.md#quic-udp-buffer-size-warning | 0.70 | Debugging › QUIC UDP Buffer Size Warning | debugging, gotchas | ",
	} {
		if !strings.Contains(resp.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, resp.Body)
		}
	}
	if strings.Contains(resp.Body, "7168 kiB.\nRestricted") {
		t.Errorf("row carries more than a one-line snippet:\n%s", resp.Body)
	}

	resp = lookup(t, h, "/", map[string]string{"query": "hairpin", "match": "body"})
	if !strings.Contains(resp.Body, "| /plain.md | 0.50 | Plain | ") {
		t.Errorf("bare-path row missing or wrong:\n%s", resp.Body)
	}

	resp = lookup(t, h, "/", map[string]string{"query": "nothing-here", "match": "body"})
	if resp.Status != protocol.StatusOK || resp.Metadata["matches"] != "0" || resp.Metadata["match"] != "body" {
		t.Errorf("empty body lookup: status %q matches %q match %q, want ok 0 body", resp.Status, resp.Metadata["matches"], resp.Metadata["match"])
	}
}

func testHandlerCatalogByteIdentical(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/docs/auth.md", "# Auth\n\nsection text\n", map[string]string{"tags": "auth", "importance": "0.9"})

	plain := lookup(t, h, "/", map[string]string{"query": "auth"})
	echoed := lookup(t, h, "/", map[string]string{"query": "auth", "match": "catalog"})
	if _, present := plain.Metadata["match"]; present {
		t.Errorf("request without match got an echo %q", plain.Metadata["match"])
	}
	if echoed.Metadata["match"] != "catalog" {
		t.Errorf("match: catalog echoed as %q", echoed.Metadata["match"])
	}
	if plain.Body != echoed.Body || !strings.Contains(plain.Body, "| Path | Importance | Title | Tags |\n") {
		t.Errorf("catalog bodies differ or lost the four-column table:\n%s\n---\n%s", plain.Body, echoed.Body)
	}
	if plain.Metadata["matches"] != "1" || echoed.Metadata["matches"] != "1" {
		t.Errorf("matches = %q / %q, want 1", plain.Metadata["matches"], echoed.Metadata["matches"])
	}
}

func testHandlerBodyUnknownMode(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	publishDoc(t, h, "/a.md", "# A\n\nfig\n", nil)
	for _, mode := range []string{"bogus", "BODY", "body,catalog"} {
		if resp := lookup(t, h, "/", map[string]string{"query": "fig", "match": mode}); resp.Status != protocol.StatusBadRequest {
			t.Errorf("match %q: status %q, want bad-request", mode, resp.Status)
		}
	}
}

func testHandlerBodyReadAuth(t *testing.T, b LookupBackend) {
	h := newReadAuthHandler(b)
	publishDoc(t, h, "/public/doc.md", "# Public\n\nrosetta stone\n", map[string]string{"importance": "0.3"})
	publishDoc(t, h, "/private/secret.md", "# Secret\n\nrosetta stone\n\n## Hidden\n\nrosetta again\n", map[string]string{"importance": "0.9"})

	resp := lookup(t, h, "/", map[string]string{"query": "rosetta", "match": "body", "limit": "1"})
	if resp.Metadata["matches"] != "1" || strings.Contains(resp.Body, "/private/") || strings.Contains(resp.Body, "Secret") {
		t.Errorf("protected sections leaked or displaced the public row:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, "/public/doc.md#public") {
		t.Errorf("public row missing:\n%s", resp.Body)
	}

	resp = lookup(t, h, "/", map[string]string{"query": "rosetta", "match": "body", "auth": readSecret})
	if resp.Metadata["matches"] != "3" || !strings.Contains(resp.Body, "/private/secret.md#hidden") {
		t.Errorf("authorized requester should see both private sections (matches %q):\n%s", resp.Metadata["matches"], resp.Body)
	}
}
