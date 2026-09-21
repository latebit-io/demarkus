package marktools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol"
)

func exploreArgs(url string) marktools.ExploreArgs {
	return marktools.ExploreArgs{URL: url, Render: mcpfmt.Options{Envelope: &mcpfmt.Fetch}}
}

func hubBackend(siblings ...string) *fetchtest.Client {
	return &fetchtest.Client{
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK, Metadata: map[string]string{"version": "2"},
				Body: "# Hub\n\nThe opening paragraph.\n\n## Part\n\n[A](/a.md)\n",
			}}, nil
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			page, next := siblings, ""
			if len(page) > r.PageSize {
				page, next = page[:r.PageSize], page[r.PageSize-1]
			}
			return fetchtest.ListPage(r.Path, next, page...), nil
		},
	}
}

func TestExploreCard(t *testing.T) {
	var seeded []string
	backend := hubBackend("a.md", "hub.md", "z.md")
	tools := newTools(t, backend, graphHooks(graphstore.New(), &seeded))

	got := tools.Explore(t.Context(), exploreArgs("/hub.md#part"))
	for _, want := range []string{
		"## Outline\n", "## Opening\nThe opening paragraph.\n", "## Outbound links (1)\n",
		"## Backlinks (0)\n", "## Siblings in / (2)\n- a.md\n- z.md\n",
		"\nfetch /hub.md#<anchor> for a section; mark_fetch force=true for the full body\n",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("card missing %q\n---\n%s", want, got.Text)
		}
	}
	if got.IsError || len(seeded) != 1 {
		t.Errorf("IsError = %v, seeded = %v", got.IsError, seeded)
	}
	sent := backend.FetchCalls[0]
	if sent.Path != "/hub.md" || sent.Token != "token-for-host:6309" {
		t.Errorf("fetch = %+v, want the document without its anchor, with the read token", sent)
	}
}

// Without a graph store the card still orients; only the graph section degrades.
func TestExploreWithoutAStoreDegrades(t *testing.T) {
	tools := newTools(t, hubBackend("hub.md"), graphHooks(nil, nil))
	got := tools.Explore(t.Context(), exploreArgs("/hub.md"))
	if got.IsError || !strings.Contains(got.Text, "## Backlinks\n(graph store unavailable)\n") || !strings.Contains(got.Text, "## Outline\n") {
		t.Errorf("card = %+v", got)
	}
}

// Twelve documents beside the hub: ten lines shown, so the card must say more
// exist. One extra entry is not enough to know, since the hub itself is one.
func TestExploreSiblingsSayWhenThereAreMore(t *testing.T) {
	names := make([]string, 0, 13)
	names = append(names, "hub.md") // sorts first, so it takes a slot on the page
	for i := range 12 {
		names = append(names, fmt.Sprintf("n-%02d.md", i))
	}
	tools := newTools(t, hubBackend(names...), graphHooks(graphstore.New(), new([]string)))
	got := tools.Explore(t.Context(), exploreArgs("/hub.md"))
	if !strings.Contains(got.Text, "more siblings") {
		_, siblings, _ := strings.Cut(got.Text, "## Siblings")
		t.Errorf("ten siblings shown of twelve, with no sign of the rest:\n%s", siblings)
	}
}
