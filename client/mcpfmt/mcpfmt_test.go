package mcpfmt

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

func fullDoc() fetch.Result {
	return fetch.Result{Response: protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"version":      "7",
			"modified":     "2026-05-21T10:00:00Z",
			"etag":         "abc-123",
			"content-hash": "sha256-deadbeef",
			"agent":        "claude-code",
			"importance":   "0.9",
			"tags":         "a,b,c",
			"title":        "Doc",
			"type":         "Reference",
			"rel-related":  "/other.md",
			"source":       "docs/x.md",
		},
		Body: "# Doc\nbody\n",
	}}
}

func TestFetchLean(t *testing.T) {
	got := Format(fullDoc(), Options{Envelope: &Fetch})
	want := "status: ok\nversion: 7\ntitle: Doc\n\n# Doc\nbody\n"
	if got != want {
		t.Fatalf("lean fetch:\n%q\nwant\n%q", got, want)
	}
}

func TestFetchVerboseIsTheFullRendering(t *testing.T) {
	got := Format(fullDoc(), Options{Envelope: &Fetch, Verbose: true})
	want := "status: ok\nversion: 7\nmodified: 2026-05-21T10:00:00Z\netag: abc-123\n" +
		"agent: claude-code\ncontent-hash: sha256-deadbeef\nimportance: 0.9\nrel-related: /other.md\n" +
		"source: docs/x.md\ntags: a,b,c\ntitle: Doc\ntype: Reference\n\n# Doc\nbody\n"
	if got != want {
		t.Fatalf("verbose fetch:\n%q\nwant\n%q", got, want)
	}
	if full := Full(fullDoc(), "version", "modified", "etag"); full != want {
		t.Fatalf("Full differs from verbose:\n%q", full)
	}
}

func TestFormatWithExtrasShowInBothModes(t *testing.T) {
	extra := map[string]string{"mode": "outline", "size": "9000 bytes, 100 lines"}
	lean := FormatWith(fullDoc(), "- outline", extra, Options{Envelope: &Fetch})
	if want := "status: ok\nversion: 7\ntitle: Doc\nmode: outline\nsize: 9000 bytes, 100 lines\n\n- outline"; lean != want {
		t.Fatalf("lean with extras:\n%q\nwant\n%q", lean, want)
	}
	verbose := FormatWith(fullDoc(), "- outline", extra, Options{Envelope: &Fetch, Verbose: true})
	for _, line := range []string{"mode: outline\n", "size: 9000 bytes, 100 lines\n", "etag: abc-123\n"} {
		if !strings.Contains(verbose, line) {
			t.Errorf("verbose with extras missing %q:\n%s", line, verbose)
		}
	}
	if strings.Contains(verbose, "body\n") {
		t.Error("replacement body not applied")
	}
	if fullDoc().Response.Metadata["mode"] != "" {
		t.Error("extras leaked into the response map")
	}
}

func TestMissingKeysAndEmptyBody(t *testing.T) {
	r := fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound, Metadata: map[string]string{}}}
	if got := Format(r, Options{Envelope: &Fetch}); got != "status: not-found\n" {
		t.Fatalf("not-found lean = %q", got)
	}
	if got := Format(r, Options{Envelope: &Fetch, Verbose: true}); got != "status: not-found\n" {
		t.Fatalf("not-found verbose = %q", got)
	}
	nilMeta := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "x"}}
	if got := FormatWith(nilMeta, "x", map[string]string{"mode": "binary"}, Options{Envelope: &Fetch, Verbose: true}); got != "status: ok\nmode: binary\n\nx" {
		t.Fatalf("nil metadata verbose = %q", got)
	}
}

func TestVerboseDeterministicOrder(t *testing.T) {
	r := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK,
		Metadata: map[string]string{"zeta": "1", "alpha": "2", "beta": "3", "delta": "4", "gamma": "5"}, Body: "body"}}
	want := "status: ok\nalpha: 2\nbeta: 3\ndelta: 4\ngamma: 5\nzeta: 1\n\nbody"
	for range 20 {
		if got := Full(r); got != want {
			t.Fatalf("Full = %q, want %q", got, want)
		}
	}
}

func TestCapTags(t *testing.T) {
	ten := "a, b, c, d, e, f, g, h, i, j"
	if got := CapTags(ten); got != ten {
		t.Fatalf("ten tags changed: %q", got)
	}
	if got := CapTags(ten + ", k, l, m"); got != ten+", +3 more" {
		t.Fatalf("thirteen tags = %q", got)
	}
	if got := CapTags(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestCapTableTags(t *testing.T) {
	many := "t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, t11, t12"
	table := "# Lookup matches for \"x\" in /\n\n" +
		"| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n" +
		"| /a.md | 0.90 | A \\| B | " + many + " |\n" +
		"| /b.md | 0.50 | B | one, two |\n"
	got := CapTableTags(table)
	want := "# Lookup matches for \"x\" in /\n\n" +
		"| Path | Importance | Title | Tags |\n|------|------------|-------|------|\n" +
		"| /a.md | 0.90 | A \\| B | t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, +2 more |\n" +
		"| /b.md | 0.50 | B | one, two |\n"
	if got != want {
		t.Fatalf("capped table:\n%q\nwant\n%q", got, want)
	}
	body := "| Path | Importance | Title | Tags | Snippet |\n|------|------------|-------|------|---------|\n" +
		"| /a.md#intro | 0.90 | A › Intro | " + many + " | snippet text |\n"
	if got := CapTableTags(body); !strings.Contains(got, "t10, +2 more | snippet text |") {
		t.Fatalf("body table not capped: %q", got)
	}
	if got := Format(fetch.Result{Response: protocol.Response{Status: protocol.StatusOK,
		Metadata: map[string]string{"matches": "1", "match": "body"}, Body: body}}, Options{Envelope: &Lookup}); !strings.HasPrefix(got, "status: ok\nmatches: 1\nmatch: body\n\n") || !strings.Contains(got, "+2 more") {
		t.Fatalf("lean lookup = %q", got)
	}
	if got := Format(fetch.Result{Response: protocol.Response{Status: protocol.StatusOK,
		Metadata: map[string]string{"matches": "1"}, Body: body}}, Options{Envelope: &Lookup, Verbose: true}); got != "status: ok\nmatches: 1\n\n"+body {
		t.Fatalf("verbose lookup = %q", got)
	}
}
