package marktools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

// mvAssertError is the relocated tests' error check: a failure whose text
// contains substr, as the surface's assertIsToolError read it.
func mvAssertError(t *testing.T, got marktools.Result, substr string) {
	t.Helper()
	if !got.IsError {
		t.Fatal("expected tool error result")
	}
	if !strings.Contains(got.Text, substr) {
		t.Errorf("error text %q does not contain %q", got.Text, substr)
	}
}

func TestListRejectsRepeatedCursor(t *testing.T) {
	backend := &fetchtest.Client{ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"entries": "0", "complete": "false", "next-cursor": "stuck",
			},
		}}, nil
	}}
	tools := (&clientSurface{}).tools(t, backend)
	got := tools.List(t.Context(), marktools.ListArgs{URL: "mark://example.com/", Cursor: "stuck"})
	mvAssertError(t, got, "did not advance")
}

func TestListRejectsMissingContinuationCursor(t *testing.T) {
	backend := &fetchtest.Client{ListFn: func(_ context.Context, _ fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK,
			Metadata: map[string]string{
				"entries": "0", "complete": "false",
			},
		}}, nil
	}}
	tools := (&clientSurface{}).tools(t, backend)
	got := tools.List(t.Context(), marktools.ListArgs{URL: "mark://example.com/"})
	mvAssertError(t, got, "missing or did not advance")
}

func TestLookupBodyMatch(t *testing.T) {
	answer := func(echo bool) *fetchtest.Client {
		return &fetchtest.Client{LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			if r.Match != fetch.MatchBody {
				return fetch.Result{}, fmt.Errorf("match = %q, want body", r.Match)
			}
			// A server without body match answers from the catalog and echoes nothing.
			match := ""
			if echo {
				match = protocol.MatchBody
			}
			return fetchtest.Lookup("hairpin", "/", match), nil
		}}
	}
	call := func(backend *fetchtest.Client) string {
		tools := (&clientSurface{DefaultHost: "mark://example.com", Token: "test-token"}).tools(t, backend)
		got := tools.Lookup(t.Context(), marktools.LookupArgs{
			URL: "mark://example.com/", Query: "hairpin", Match: "body",
			Render: mcpfmt.Options{Envelope: &mcpfmt.Lookup},
		})
		if got.IsError {
			t.Fatalf("Lookup: result %+v", got)
		}
		return got.Text
	}
	echoed := call(answer(true))
	if !strings.Contains(echoed, "match: body") || strings.Contains(echoed, fetch.CatalogFallbackNote) {
		t.Errorf("echoed body answer wrong:\n%s", echoed)
	}
	silent := call(answer(false))
	if !strings.Contains(silent, fetch.CatalogFallbackNote) {
		t.Errorf("catalog fallback not flagged:\n%s", silent)
	}
}
