package main

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/mark3labs/mcp-go/mcp"
)

// mark_fetch's modes (outline, section, binary, dedup, notes) are tested where
// they live, in client/marktools. Here: the handler's arguments reach that
// body, and this server's seen store lasts for the process.

// fetchStub returns a stubClient serving one document body with version/etag
// metadata, counting fetches.
func fetchStub(body, version, etag string, calls *int) *stubClient {
	return &stubClient{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
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

// url with its #anchor, force and verbose all have to arrive at the body.
func TestHandlerMarkFetch_ArgumentsReachTheBody(t *testing.T) {
	calls := 0
	h := &handler{client: fetchStub(bigDoc(), "3", "abc", &calls)}

	outline := fetchText(t, h, map[string]any{"url": "mark://example.com/big.md"})
	if !strings.Contains(outline, "mode: outline") || strings.Contains(outline, "etag: abc\n") {
		t.Fatalf("default call should outline with a lean envelope, got:\n%.300s", outline)
	}
	forced := fetchText(t, h, map[string]any{"url": "mark://example.com/big.md", "force": true, "verbose": true})
	if strings.Contains(forced, "mode: outline") || !strings.Contains(forced, "filler line") || !strings.Contains(forced, "etag: abc\n") {
		t.Errorf("force and verbose did not reach the body:\n%.300s", forced)
	}
	section := fetchText(t, h, map[string]any{"url": "mark://example.com/big.md#usage"})
	if !strings.Contains(section, "section: #usage") {
		t.Errorf("the #anchor did not reach the body:\n%.300s", section)
	}
	if calls != 3 {
		t.Errorf("fetches = %d, want one per call", calls)
	}
}

// The stdio server is one agent's process: what one call returned in full,
// a later call on the same handler reports as unchanged.
func TestHandlerMarkFetch_SeenStoreLastsForTheProcess(t *testing.T) {
	h := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	url := map[string]any{"url": "mark://example.com/doc.md"}

	if first := fetchText(t, h, url); !strings.Contains(first, "Setup body.") {
		t.Fatal("first fetch should return full body")
	}
	if second := fetchText(t, h, url); !strings.Contains(second, "status: unchanged") {
		t.Fatalf("second fetch on the same handler should dedup, got:\n%s", second)
	}
	other := &handler{client: fetchStub(smallDoc, "3", "abc", nil)}
	if fresh := fetchText(t, other, url); !strings.Contains(fresh, "Setup body.") {
		t.Errorf("another process has seen nothing, got:\n%s", fresh)
	}
}

// mark_lookup's verbose flag picks the envelope; the tag cap itself is pinned
// in client/marktools.
func TestHandlerMarkLookup_VerboseReachesTheBody(t *testing.T) {
	many := []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9", "t10", "t11", "t12"}
	answer := fetchtest.Lookup("x", "/", "", render.LookupRow{Path: "/a.md", Importance: 0.9, Title: "A", Tags: many})
	h := &handler{client: &stubClient{LookupFn: func(_ context.Context, _ fetch.LookupRequest) (fetch.Result, error) {
		return answer, nil
	}}}
	result, err := h.markLookup(context.Background(), newCallToolRequest(map[string]any{"url": "mark://example.com/", "query": "x", "verbose": true}))
	if err != nil || result.IsError {
		t.Fatalf("lookup failed: %v %v", err, result)
	}
	if got := result.Content[0].(mcp.TextContent).Text; got != "status: ok\nmatches: 1\n\n"+answer.Response.Body {
		t.Fatalf("verbose lookup:\n%s", got)
	}
}
