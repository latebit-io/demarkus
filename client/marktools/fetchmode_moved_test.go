package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
)

// fmStub serves one document body with version and etag metadata.
func fmStub(body, version, etag string) *fetchtest.Client {
	return &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": version, "modified": "2026-07-04T00:00:00Z", "etag": etag},
				Body:     body,
			}}, nil
		},
	}
}

// fmArgs are one mark_fetch call as an agent would make it.
type fmArgs struct {
	url            string
	force, verbose bool
}

func (a fmArgs) fetchArgs() marktools.FetchArgs {
	return marktools.FetchArgs{URL: a.url, Force: a.force, Render: mcpfmt.Options{Envelope: &mcpfmt.Fetch, Verbose: a.verbose}}
}

func fmText(t *testing.T, tools *marktools.Tools, args fmArgs) string {
	t.Helper()
	result := tools.Fetch(context.Background(), args.fetchArgs())
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Text)
	}
	return result.Text
}

func fmTools(t *testing.T, backend marktools.Backend) *marktools.Tools {
	t.Helper()
	return (&clientSurface{}).tools(t, backend)
}

const fmSmallDoc = "# Doc\n\nIntro paragraph.\n\n## Setup\n\nSetup body.\n\n## Usage\n\nUsage body.\n"

// fmBigDoc is a document over the outline threshold with the same structure.
func fmBigDoc() string {
	filler := strings.Repeat("filler line for section body padding\n", 150)
	return "# Big\n\nIntro paragraph.\n\n## Setup\n\n" + filler + "\n## Usage\n\n" + filler
}

func TestFetch_SmallDocFullBody(t *testing.T) {
	tools := fmTools(t, fmStub(fmSmallDoc, "3", "abc"))
	text := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md"})
	if !strings.Contains(text, "Setup body.") || !strings.Contains(text, "Usage body.") {
		t.Errorf("small doc should return full body, got:\n%s", text)
	}
	if strings.Contains(text, "mode: outline") {
		t.Error("small doc should not be outlined")
	}
}

func TestFetch_LargeDocOutline(t *testing.T) {
	tools := fmTools(t, fmStub(fmBigDoc(), "3", "abc"))
	text := fmText(t, tools, fmArgs{url: "mark://example.com/big.md"})

	if !strings.Contains(text, "mode: outline") {
		t.Fatalf("large doc should return outline, got:\n%s", text[:200])
	}
	if strings.Contains(text, "filler line") {
		t.Error("outline should not include section bodies")
	}
	for _, want := range []string{"#setup", "#usage", "Intro paragraph.", "fetch mark://example.com/big.md#<anchor> for a section", "size: "} {
		if !strings.Contains(text, want) {
			t.Errorf("outline missing %q in:\n%s", want, text)
		}
	}
}

func TestFetch_LargeDocForceFullBody(t *testing.T) {
	tools := fmTools(t, fmStub(fmBigDoc(), "3", "abc"))
	text := fmText(t, tools, fmArgs{url: "mark://example.com/big.md", force: true})
	if strings.Contains(text, "mode: outline") {
		t.Error("force=true should bypass outline mode")
	}
	if !strings.Contains(text, "filler line") {
		t.Error("force=true should return the full body")
	}
}

// fmBinaryDoc is a non-UTF-8 body (PNG magic), standing in for legacy or
// out-of-band binary a fetch might still encounter.
const fmBinaryDoc = "\x89PNG\r\n\x1a\n\xff\xfe\x00\xb1\xa5"

func TestFetch_BinaryNotice(t *testing.T) {
	tools := fmTools(t, fmStub(fmBinaryDoc, "1", "abc"))
	text := fmText(t, tools, fmArgs{url: "mark://example.com/img.png"})
	if !strings.Contains(text, "mode: binary") {
		t.Errorf("binary body should be flagged mode: binary, got:\n%s", text)
	}
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("binary body should return the notice, got:\n%s", text)
	}
	if strings.Contains(text, "\x89PNG") {
		t.Error("binary bytes must not be rendered into the response")
	}
}

func TestFetch_BinaryForceStillNotice(t *testing.T) {
	// force bypasses the size gate, never the binary gate: MCP text cannot carry
	// binary faithfully, so force=true must still return the notice, not bytes.
	tools := fmTools(t, fmStub(fmBinaryDoc, "1", "abc"))
	text := fmText(t, tools, fmArgs{url: "mark://example.com/img.png", force: true})
	if !strings.Contains(text, "non-markdown or binary document") {
		t.Errorf("force=true must still return the binary notice, got:\n%s", text)
	}
	if strings.Contains(text, "\x89PNG") {
		t.Error("force=true must not leak raw binary bytes")
	}
}

func TestFetch_SectionSlice(t *testing.T) {
	tests := []struct {
		name   string
		url    string
		wantIn []string
		notIn  []string
	}{
		{
			"section of small doc",
			"mark://example.com/doc.md#setup",
			[]string{"section: #setup", "## Setup", "Setup body."},
			[]string{"Usage body."},
		},
		{
			"section works on large doc too",
			"mark://example.com/doc.md#usage",
			[]string{"## Usage"},
			[]string{"## Setup", "mode: outline"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmSmallDoc
			if strings.Contains(tt.name, "large") {
				body = fmBigDoc()
			}
			tools := fmTools(t, fmStub(body, "3", "abc"))
			text := fmText(t, tools, fmArgs{url: tt.url})
			for _, want := range tt.wantIn {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			for _, not := range tt.notIn {
				if strings.Contains(text, not) {
					t.Errorf("unexpected %q in:\n%s", not, text)
				}
			}
		})
	}
}

func TestFetch_SectionNotFound(t *testing.T) {
	tools := fmTools(t, fmStub(fmSmallDoc, "3", "abc"))
	result := tools.Fetch(context.Background(), fmArgs{url: "mark://example.com/doc.md#nope"}.fetchArgs())
	if !result.IsError {
		t.Fatal("expected tool error for missing section")
	}
	if !strings.Contains(result.Text, "available anchors") || !strings.Contains(result.Text, "setup") {
		t.Errorf("error should list available anchors, got: %s", result.Text)
	}
}

func TestFetch_SessionDedup(t *testing.T) {
	tools := fmTools(t, fmStub(fmSmallDoc, "3", "abc"))
	url := fmArgs{url: "mark://example.com/doc.md"}

	first := fmText(t, tools, url)
	if !strings.Contains(first, "Setup body.") {
		t.Fatal("first fetch should return full body")
	}

	second := fmText(t, tools, url)
	if !strings.Contains(second, "status: unchanged") {
		t.Fatalf("second fetch should dedup, got:\n%s", second)
	}
	if !strings.Contains(second, "unchanged since v3") || !strings.Contains(second, "force=true") {
		t.Errorf("dedup notice should name the version and the override, got:\n%s", second)
	}
	if strings.Contains(second, "Setup body.") {
		t.Error("dedup response should not carry the body")
	}

	forced := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md", force: true})
	if !strings.Contains(forced, "Setup body.") {
		t.Error("force=true should bust the dedup and return the body")
	}
}

func TestFetch_SectionFetchBypassesDedup(t *testing.T) {
	tools := fmTools(t, fmStub(fmSmallDoc, "3", "abc"))
	_ = fmText(t, tools, fmArgs{url: "mark://example.com/doc.md"})
	text := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md#setup"})
	if !strings.Contains(text, "Setup body.") {
		t.Errorf("section fetch after full fetch must return content, got:\n%s", text)
	}
}

// fmIdentityStub serves fmSmallDoc under an identity the test can change.
func fmIdentityStub(version, etag *string) *fetchtest.Client {
	return &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": *version, "etag": *etag},
				Body:     fmSmallDoc,
			}}, nil
		},
	}
}

func TestFetch_ChangedDocNoted(t *testing.T) {
	version, etag := "3", "abc"
	tools := fmTools(t, fmIdentityStub(&version, &etag))
	url := fmArgs{url: "mark://example.com/doc.md"}

	_ = fmText(t, tools, url)
	version, etag = "5", "def"
	text := fmText(t, tools, url)
	if !strings.Contains(text, "note: changed since this session's earlier fetch (v3 -> v5)") {
		t.Errorf("changed doc should carry a version delta note, got:\n%s", text)
	}
	if !strings.Contains(text, "Setup body.") {
		t.Error("changed doc should return the body")
	}

	// The new version is now the recorded one.
	third := fmText(t, tools, url)
	if !strings.Contains(third, "unchanged since v5") {
		t.Errorf("third fetch should dedup at the new version, got:\n%s", third)
	}
}

func TestFetch_NoIdentityNoDedup(t *testing.T) {
	// A server that omits both version and etag gives dedup nothing to
	// compare: every fetch must return the body, never "unchanged".
	tools := fmTools(t, fmStub(fmSmallDoc, "", ""))
	url := fmArgs{url: "mark://example.com/doc.md"}

	for range 2 {
		text := fmText(t, tools, url)
		if strings.Contains(text, "status: unchanged") {
			t.Fatalf("fetch without version/etag must not dedup, got:\n%s", text)
		}
		if !strings.Contains(text, "Setup body.") {
			t.Errorf("fetch should return the body, got:\n%s", text)
		}
	}
}

func TestFetch_EtagOnlyChangeNoted(t *testing.T) {
	version, etag := "3", "abc"
	tools := fmTools(t, fmIdentityStub(&version, &etag))
	url := fmArgs{url: "mark://example.com/doc.md"}

	_ = fmText(t, tools, url)
	etag = "def"
	text := fmText(t, tools, url)
	if strings.Contains(text, "status: unchanged") {
		t.Fatal("etag change must bypass the dedup")
	}
	if !strings.Contains(text, "note: content changed since this session's earlier fetch (still v3, etag differs)") {
		t.Errorf("etag-only change should carry a note, got:\n%s", text)
	}
	if !strings.Contains(text, "Setup body.") {
		t.Error("etag-only change should return the body")
	}
}

// Identity delta wording (unchanged notice, changed note, asymmetric identity
// flips) is pinned once in client/fetchdedup's tests; every surface renders
// through that package.

func TestFetch_IdentityLostNoNote(t *testing.T) {
	// A response that loses both identity fields after an identified fetch has
	// nothing truthful to say about what changed: body, no note, no dedup.
	version, etag := "3", "abc"
	tools := fmTools(t, fmIdentityStub(&version, &etag))
	url := fmArgs{url: "mark://example.com/doc.md"}

	_ = fmText(t, tools, url)
	version, etag = "", ""
	text := fmText(t, tools, url)
	if strings.Contains(text, "note:") {
		t.Errorf("identity-less response must not carry a changed note, got:\n%s", text)
	}
	if !strings.Contains(text, "Setup body.") {
		t.Error("identity-less response should return the body")
	}
}

func TestFetch_OutlineDoesNotRecordSeen(t *testing.T) {
	tools := fmTools(t, fmStub(fmBigDoc(), "3", "abc"))
	url := fmArgs{url: "mark://example.com/big.md"}

	first := fmText(t, tools, url)
	if !strings.Contains(first, "mode: outline") {
		t.Fatal("expected outline")
	}
	// A repeat fetch returns the outline again, never "unchanged": the agent
	// has not seen the full body yet.
	second := fmText(t, tools, url)
	if strings.Contains(second, "status: unchanged") {
		t.Error("outline-only fetch must not arm the dedup")
	}
	if !strings.Contains(second, "mode: outline") {
		t.Error("repeat fetch of a large doc should outline again")
	}
}

func TestFetch_NonOKStatusPassthrough(t *testing.T) {
	backend := &fetchtest.Client{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusNotFound,
				Metadata: map[string]string{},
			}}, nil
		},
	}
	text := fmText(t, fmTools(t, backend), fmArgs{url: "mark://example.com/missing.md#setup"})
	if !strings.Contains(text, "status: not-found") {
		t.Errorf("non-ok status should pass through, got:\n%s", text)
	}
}

func TestFetch_LeanEnvelopeAndVerbose(t *testing.T) {
	tools := fmTools(t, &fetchtest.Client{FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{"version": "3", "modified": "2026-07-04T00:00:00Z", "etag": "abc",
				"content-hash": "sha256-1", "agent": "x", "tags": "a,b", "title": "Doc", "type": "Note"},
			Body: fmSmallDoc,
		}}, nil
	}})
	lean := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md"})
	if !strings.HasPrefix(lean, "status: ok\nversion: 3\ntitle: Doc\n\n# Doc") {
		t.Fatalf("lean envelope:\n%s", lean)
	}
	verbose := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md", verbose: true, force: true})
	for _, want := range []string{"etag: abc\n", "content-hash: sha256-1\n", "tags: a,b\n", "type: Note\n", "modified: "} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose envelope missing %q:\n%s", want, verbose)
		}
	}
	unchanged := fmText(t, tools, fmArgs{url: "mark://example.com/doc.md"})
	if !strings.HasPrefix(unchanged, "status: unchanged\nversion: 3\n\n") {
		t.Fatalf("lean unchanged notice:\n%s", unchanged)
	}
}

func TestLookup_TagCapAndVerbose(t *testing.T) {
	many := []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12"}
	answer := fetchtest.Lookup("x", "/", "", render.LookupRow{Path: "/a.md", Importance: 0.9, Title: "A", Tags: many})
	tools := fmTools(t, &fetchtest.Client{LookupFn: func(_ context.Context, _ fetch.LookupRequest) (fetch.Result, error) {
		return answer, nil
	}})
	call := func(verbose bool) string {
		t.Helper()
		result := tools.Lookup(context.Background(), marktools.LookupArgs{
			URL: "mark://example.com/", Query: "x", Render: mcpfmt.Options{Envelope: &mcpfmt.Lookup, Verbose: verbose},
		})
		if result.IsError {
			t.Fatalf("lookup failed: %v", result)
		}
		return result.Text
	}
	lean := call(false)
	if !strings.Contains(lean, "| t1, t2, t3, t4, t5, t6, t7, t8, t9, t10, +2 more |") {
		t.Fatalf("lean lookup not capped:\n%s", lean)
	}
	verbose := call(true)
	if verbose != "status: ok\nmatches: 1\n\n"+answer.Response.Body {
		t.Fatalf("verbose lookup:\n%s", verbose)
	}
}

func TestPublish_NarrowingNote(t *testing.T) {
	// A fresh backend per publish: it stores each publish as the new current
	// version, which would otherwise conflict the next case.
	publish := func(version int, meta map[string]any, onConflict string) string {
		t.Helper()
		current := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK,
			Metadata: map[string]string{"version": "3", "etag": "e", "tags": "a,b,c", "type": "Note", "title": "T"}, Body: "x"}}
		backend := &fetchtest.Client{
			Published: map[string]fetch.Result{
				"example.com:6309/doc.md":                                 current,
				"example.com:6309" + generation.VersionPath("/doc.md", 3): current,
			},
			PublishFn: func(_ context.Context, _ fetch.WriteRequest) (fetch.Result, error) {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "4"}}}, nil
			},
		}
		if version == 0 {
			backend.Published = nil
		}
		tools := (&clientSurface{DefaultHost: "mark://example.com", Token: "test"}).tools(t, backend)
		result := tools.Publish(context.Background(), marktools.PublishArgs{
			URL: "mark://example.com/doc.md", Body: "y", ExpectedVersion: &version,
			Metadata: meta, OnConflict: onConflict,
		})
		if result.IsError {
			t.Fatalf("publish failed: %v", result)
		}
		return result.Text
	}
	for _, mode := range []string{"fail", "merge"} {
		narrowed := publish(3, map[string]any{"tags": "a", "title": "T"}, mode)
		if !strings.HasPrefix(narrowed, "status: ok\nversion: 4\n") ||
			!strings.Contains(narrowed, "\nnote: this publish dropped tags b, c and keys type=Note carried by v3;") {
			t.Errorf("%s: narrowing note missing:\n%s", mode, narrowed)
		}
	}
	if full := publish(3, map[string]any{"tags": "c,b,a", "title": "T", "type": "Note"}, "fail"); strings.Contains(full, "note:") {
		t.Errorf("complete metadata noted:\n%s", full)
	}
	if created := publish(0, map[string]any{"tags": "a"}, "fail"); strings.Contains(created, "note:") {
		t.Errorf("create noted:\n%s", created)
	}
}
