package main

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/mark3labs/mcp-go/mcp"
)

// The explore card (sections, caps, backlinks, relations, degradation) is
// tested where it lives, in client/marktools. Here: required arguments, this
// server's URL rule, and that every argument reaches the body.

func exploreStub() *stubClient {
	return &stubClient{
		FetchFn: func(_ context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"version": "2", "modified": "2026-07-04T00:00:00Z", "etag": "xyz"},
				Body:     "# Hub\n\nThe hub links everything together.\n\n- [Alpha](/alpha.md)\n- [Beta](/docs/beta.md)\n",
			}}, nil
		},
		ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage("/", "", "alpha.md", "hub.md"), nil
		},
	}
}

func TestHandlerMarkExplore_MissingURL(t *testing.T) {
	h := &handler{client: &stubClient{}}
	result, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "url is required")
}

func TestHandlerMarkExplore_InvalidURL(t *testing.T) {
	h := &handler{client: &stubClient{}}
	result, err := h.markExplore(context.Background(), newCallToolRequest(map[string]any{"url": "/bare"}))
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	assertIsToolError(t, result, "requires -host flag")
}

// A url alone gets the default card; a relation argument switches the graph
// section, and direction, relations, page_size and verbose all arrive.
func TestHandlerMarkExplore_ArgumentsReachTheBody(t *testing.T) {
	h := &handler{client: exploreStub(), graphStore: graphstore.New()}
	explore := func(args map[string]any) string {
		t.Helper()
		result, err := h.markExplore(context.Background(), newCallToolRequest(args))
		if err != nil || result.IsError {
			t.Fatalf("markExplore: err=%v result=%+v", err, result)
		}
		return result.Content[0].(mcp.TextContent).Text
	}

	card := explore(map[string]any{"url": "mark://host:6309/hub.md"})
	if !strings.Contains(card, "## Backlinks (0)") || strings.Contains(card, "## Relations") || strings.Contains(card, "etag: xyz\n") {
		t.Fatalf("default card:\n%s", card)
	}
	relations := explore(map[string]any{
		"url": "mark://host:6309/hub.md", "direction": "outgoing", "page_size": 1, "verbose": true,
	})
	for _, want := range []string{"## Relations (2 documents)", "mark://host/alpha.md", "next-cursor: ", "etag: xyz\n"} {
		if !strings.Contains(relations, want) {
			t.Errorf("relation card missing %q:\n%s", want, relations)
		}
	}
	if strings.Contains(relations, "mark://host/docs/beta.md") {
		t.Errorf("page_size did not reach the body:\n%s", relations)
	}
}
