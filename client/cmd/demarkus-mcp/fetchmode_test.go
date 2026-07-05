package main

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// fetchStub returns a stubClient serving one document body with version/etag
// metadata, counting fetches.
func fetchStub(body, version, etag string, calls *int) *stubClient {
	return &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			if calls != nil {
				*calls++
			}
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": version, "modified": "2026-07-04T00:00:00Z", "etag": etag},
				Body:     body,
			}}, nil
		},
	}
}

func fetchText(t *testing.T, h *handler, args map[string]any) string {
	t.Helper()
	result, err := h.markFetch(context.Background(), newCallToolRequest(args))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %v", result.Content)
	}
	return result.Content[0].(mcp.TextContent).Text
}

const smallDoc = "# Doc\n\nIntro paragraph.\n\n## Setup\n\nSetup body.\n\n## Usage\n\nUsage body.\n"

// bigDoc is a document over the outline threshold with the same structure.
func bigDoc() string {
	filler := strings.Repeat("filler line for section body padding\n", 150)
	return "# Big\n\nIntro paragraph.\n\n## Setup\n\n" + filler + "\n## Usage\n\n" + filler
}

func TestHandlerMarkFetch_SmallDocFullBody(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	text := fetchText(t, h, map[string]any{"url": "mark://example.com/doc.md"})
	if !strings.Contains(text, "Setup body.") || !strings.Contains(text, "Usage body.") {
		t.Errorf("small doc should return full body, got:\n%s", text)
	}
	if strings.Contains(text, "mode: outline") {
		t.Error("small doc should not be outlined")
	}
}

func TestHandlerMarkFetch_LargeDocOutline(t *testing.T) {
	h := &handler{client: fetchStub(bigDoc(), "3", "abc", nil)}
	text := fetchText(t, h, map[string]any{"url": "mark://example.com/big.md"})

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

func TestHandlerMarkFetch_LargeDocForceFullBody(t *testing.T) {
	h := &handler{client: fetchStub(bigDoc(), "3", "abc", nil)}
	text := fetchText(t, h, map[string]any{"url": "mark://example.com/big.md", "force": true})
	if strings.Contains(text, "mode: outline") {
		t.Error("force=true should bypass outline mode")
	}
	if !strings.Contains(text, "filler line") {
		t.Error("force=true should return the full body")
	}
}

func TestHandlerMarkFetch_SectionSlice(t *testing.T) {
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
			body := smallDoc
			if strings.Contains(tt.name, "large") {
				body = bigDoc()
			}
			h := &handler{client: fetchStub(body, "3", "abc", nil)}
			text := fetchText(t, h, map[string]any{"url": tt.url})
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

func TestHandlerMarkFetch_SectionNotFound(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	result, err := h.markFetch(context.Background(), newCallToolRequest(map[string]any{
		"url": "mark://example.com/doc.md#nope",
	}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected tool error for missing section")
	}
	text := result.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "available anchors") || !strings.Contains(text, "setup") {
		t.Errorf("error should list available anchors, got: %s", text)
	}
}

func TestHandlerMarkFetch_SessionDedup(t *testing.T) {
	calls := 0
	h := &handler{client: fetchStub(smallDoc, "3", "abc", &calls)}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	first := fetchText(t, h, url)
	if !strings.Contains(first, "Setup body.") {
		t.Fatal("first fetch should return full body")
	}

	second := fetchText(t, h, url)
	if !strings.Contains(second, "status: unchanged") {
		t.Fatalf("second fetch should dedup, got:\n%s", second)
	}
	if !strings.Contains(second, "unchanged since v3") || !strings.Contains(second, "force=true") {
		t.Errorf("dedup notice should name the version and the override, got:\n%s", second)
	}
	if strings.Contains(second, "Setup body.") {
		t.Error("dedup response should not carry the body")
	}

	forced := fetchText(t, h, map[string]any{"url": "mark://example.com/doc.md", "force": true})
	if !strings.Contains(forced, "Setup body.") {
		t.Error("force=true should bust the dedup and return the body")
	}
}

func TestHandlerMarkFetch_SectionFetchBypassesDedup(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	_ = fetchText(t, h, map[string]any{"url": "mark://example.com/doc.md"})
	text := fetchText(t, h, map[string]any{"url": "mark://example.com/doc.md#setup"})
	if !strings.Contains(text, "Setup body.") {
		t.Errorf("section fetch after full fetch must return content, got:\n%s", text)
	}
}

func TestHandlerMarkFetch_ChangedDocNoted(t *testing.T) {
	version, etag := "3", "abc"
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": version, "etag": etag},
				Body:     smallDoc,
			}}, nil
		},
	}
	h := &handler{client: sc}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	_ = fetchText(t, h, url)
	version, etag = "5", "def"
	text := fetchText(t, h, url)
	if !strings.Contains(text, "note: changed since this session's earlier fetch (v3 -> v5)") {
		t.Errorf("changed doc should carry a version delta note, got:\n%s", text)
	}
	if !strings.Contains(text, "Setup body.") {
		t.Error("changed doc should return the body")
	}

	// The new version is now the recorded one.
	third := fetchText(t, h, url)
	if !strings.Contains(third, "unchanged since v5") {
		t.Errorf("third fetch should dedup at the new version, got:\n%s", third)
	}
}

func TestHandlerMarkFetch_NoIdentityNoDedup(t *testing.T) {
	// A server that omits both version and etag gives dedup nothing to
	// compare — every fetch must return the body, never "unchanged".
	h := &handler{client: fetchStub(smallDoc, "", "", nil)}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	for range 2 {
		text := fetchText(t, h, url)
		if strings.Contains(text, "status: unchanged") {
			t.Fatalf("fetch without version/etag must not dedup, got:\n%s", text)
		}
		if !strings.Contains(text, "Setup body.") {
			t.Errorf("fetch should return the body, got:\n%s", text)
		}
	}
}

func TestHandlerMarkFetch_EtagOnlyChangeNoted(t *testing.T) {
	etag := "abc"
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "3", "etag": etag},
				Body:     smallDoc,
			}}, nil
		},
	}
	h := &handler{client: sc}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	_ = fetchText(t, h, url)
	etag = "def"
	text := fetchText(t, h, url)
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

// Identity-delta wording (unchanged notice, changed note, asymmetric
// identity flips) is pinned once in client/fetchdedup's tests — both this
// binary and the broker gateway render through that package.

func TestHandlerMarkFetch_IdentityLostNoNote(t *testing.T) {
	// A response that loses BOTH identity fields after an identified
	// fetch has nothing truthful to say about what changed — the body
	// comes back with no note (and no dedup).
	version, etag := "3", "abc"
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": version, "etag": etag},
				Body:     smallDoc,
			}}, nil
		},
	}
	h := &handler{client: sc}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	_ = fetchText(t, h, url)
	version, etag = "", ""
	text := fetchText(t, h, url)
	if strings.Contains(text, "note:") {
		t.Errorf("identity-less response must not carry a changed note, got:\n%s", text)
	}
	if !strings.Contains(text, "Setup body.") {
		t.Error("identity-less response should return the body")
	}
}

func TestHandlerMarkFetch_OutlineDoesNotRecordSeen(t *testing.T) {
	h := &handler{client: fetchStub(bigDoc(), "3", "abc", nil)}
	url := map[string]any{"url": "mark://example.com/big.md"}

	first := fetchText(t, h, url)
	if !strings.Contains(first, "mode: outline") {
		t.Fatal("expected outline")
	}
	// A repeat fetch returns the outline again, never "unchanged" — the
	// agent has not seen the full body yet.
	second := fetchText(t, h, url)
	if strings.Contains(second, "status: unchanged") {
		t.Error("outline-only fetch must not arm the dedup")
	}
	if !strings.Contains(second, "mode: outline") {
		t.Error("repeat fetch of a large doc should outline again")
	}
}

func TestHandlerMarkFetch_NonOKStatusPassthrough(t *testing.T) {
	sc := &stubClient{
		fetchFn: func(_, _, _ string) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusNotFound,
				Metadata: map[string]string{},
			}}, nil
		},
	}
	h := &handler{client: sc}
	text := fetchText(t, h, map[string]any{"url": "mark://example.com/missing.md#setup"})
	if !strings.Contains(text, "status: not-found") {
		t.Errorf("non-ok status should pass through, got:\n%s", text)
	}
}
