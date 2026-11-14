package storetest

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
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
		{"BodyFallbackEchoesCatalog", testHandlerBodyFallback},
		{"PinsColumn", testHandlerPinsColumn},
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
	return sendSettled(t, h, request(protocol.VerbLookup, scope, meta, ""))
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

// testHandlerPinsColumn: pins: true adds the Pins column, the values a
// based-on filter matches or all of them; a bad value is refused.
func testHandlerPinsColumn(t *testing.T, b LookupBackend) {
	h := NewHandler(b)
	pins := "https://github.com/o/r/tree/abc1234/auth, /notes/src.md/v2, https://github.com/o/other/commit/def5678"
	publishDoc(t, h, "/notes/auth.md", "# Auth\n\nsection text\n", map[string]string{"tags": "auth", "rel-based-on": pins})

	resp := lookup(t, h, "/", map[string]string{"query": "*", "filter": "based-on=https://github.com/o/r", "pins": "true"})
	for _, want := range []string{
		"| Path | Importance | Title | Tags | Pins |",
		"| /notes/auth.md | 0.50 | Auth | auth | https://github.com/o/r/tree/abc1234/auth |\n",
	} {
		if !strings.Contains(resp.Body, want) {
			t.Errorf("based-on pins: body lacks %q:\n%s", want, resp.Body)
		}
	}

	resp = lookup(t, h, "/", map[string]string{"query": "auth", "pins": "true"})
	if want := "| auth | https://github.com/o/r/tree/abc1234/auth, /notes/src.md/v2, https://github.com/o/other/commit/def5678 |\n"; !strings.Contains(resp.Body, want) {
		t.Errorf("all pins: body lacks %q:\n%s", want, resp.Body)
	}

	resp = lookup(t, h, "/", map[string]string{"query": "section", "match": "body", "pins": "true"})
	if !strings.Contains(resp.Body, "| Path | Importance | Title | Tags | Snippet | Pins |") || !strings.Contains(resp.Body, "| section text | "+pins+" |\n") {
		t.Errorf("body mode: Pins does not follow Snippet:\n%s", resp.Body)
	}

	resp = lookup(t, h, "/", map[string]string{"query": "*", "filter": "based-on=/elsewhere/", "pins": "true"})
	if resp.Metadata["matches"] != "0" || !strings.Contains(resp.Body, "| Path | Importance | Title | Tags | Pins |") {
		t.Errorf("empty result lost the Pins header: matches %q\n%s", resp.Metadata["matches"], resp.Body)
	}

	if resp := lookup(t, h, "/", map[string]string{"query": "auth", "pins": "yes"}); resp.Status != protocol.StatusBadRequest {
		t.Errorf("pins yes: status %q, want bad-request", resp.Status)
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

// FallbackStore wraps a store so every section-index lookup mode answers
// ErrBodyMatchUnavailable, as a bucket store does before its index covers
// the snapshot; catalog mode answers as before.
type FallbackStore struct{ handler.DocumentStore }

type fallbackStore = FallbackStore

func (s fallbackStore) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	view, err := s.DocumentStore.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return fallbackView{view}, nil
}

type fallbackView struct{ backend.ReadView }

// Lookup answers catalog mode only: a section-index mode is refused as a
// bucket store does before its index covers the snapshot.
func (v fallbackView) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	if protocol.MatchSectioned(string(opts.Match)) {
		return nil, backend.ErrBodyMatchUnavailable
	}
	return v.ReadView.Lookup(ctx, query, opts)
}

func testHandlerBodyFallback(t *testing.T, b LookupBackend) {
	h := NewHandler(LookupBackend{Store: fallbackStore{b.Store}})
	publishDoc(t, h, "/docs/doc.md", "# Doc\n\n## Section\n\npapaya\n", map[string]string{"tags": "fruit", "importance": "0.6"})
	// A similar request without a section index has no catalog answer: an
	// empty table that says so.
	if resp := Send(t, h, request(protocol.VerbLookup, "/", map[string]string{"match": "similar"}, neardtest.RewordedDoc)); resp.Status != protocol.StatusOK || resp.Metadata["match"] != "catalog" || resp.Metadata["matches"] != "0" {
		t.Errorf("similar fallback: %s match %q matches %q, want ok catalog 0", resp.Status, resp.Metadata["match"], resp.Metadata["matches"])
	}

	for _, scope := range []string{"/", "/docs"} {
		body := Send(t, h, request(protocol.VerbLookup, scope, map[string]string{"query": "fruit", "match": "body"}, ""))
		plain := Send(t, h, request(protocol.VerbLookup, scope, map[string]string{"query": "fruit", "match": "catalog"}, ""))
		if body.Status != protocol.StatusOK || body.Metadata["match"] != "catalog" || body.Metadata["matches"] != "1" {
			t.Errorf("scope %s: status %q match %q matches %q, want ok catalog 1 (%s)", scope, body.Status, body.Metadata["match"], body.Metadata["matches"], body.Body)
		}
		if body.Body != plain.Body {
			t.Errorf("scope %s: fallback answer differs from the catalog answer:\n%s\n---\n%s", scope, body.Body, plain.Body)
		}
	}
	// A term only the body holds finds nothing in the catalog, and the echo
	// says so instead of claiming the body was searched.
	resp := Send(t, h, request(protocol.VerbLookup, "/", map[string]string{"query": "papaya", "match": "body"}, ""))
	if resp.Metadata["match"] != "catalog" || resp.Metadata["matches"] != "0" || strings.Contains(resp.Body, "Snippet") {
		t.Errorf("body-only term: match %q matches %q, want catalog 0 and four columns:\n%s", resp.Metadata["match"], resp.Metadata["matches"], resp.Body)
	}
}
