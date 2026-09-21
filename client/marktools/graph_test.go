package marktools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// graphHooks is a surface with one graph store; targets are keyed by identity.
func graphHooks(store *graphstore.Store, seeded *[]string) marktools.Hooks {
	hooks := writingHooks()
	hooks.Resolve = func(_ context.Context, raw string) (marktools.Target, error) {
		return marktools.Target{Host: "host:6309", Path: raw, NodeURL: "mark://host" + raw}, nil
	}
	hooks.Graph = func(context.Context) (*marktools.GraphScope, error) {
		if store == nil {
			return nil, errors.New("graph store not available")
		}
		return &marktools.GraphScope{
			Store: store,
			Fetch: func(_ context.Context, target links.Target) (graph.FetchResult, error) {
				body := "# " + target.Path + "\n"
				if target.Path == "/a.md" {
					body += "\n[Page C](/c.md)\n" // the source still holds its link when revalidated
				}
				return graph.FetchResult{Status: protocol.StatusOK, Body: body}, nil
			},
			Seed:      func(_ context.Context, target marktools.Target) { *seeded = append(*seeded, target.Host) },
			EmptyHint: "Run mark_graph to populate the graph store.",
		}, nil
	}
	return hooks
}

func linkedStore() *graphstore.Store {
	g := graph.New()
	g.AddNode(&graph.Node{URL: "mark://host/a.md", Title: "Page A", Status: "ok"})
	g.AddNode(&graph.Node{URL: "mark://host/c.md", Title: "Page C", Status: "ok"})
	g.AddEdge("mark://host/a.md", "mark://host/c.md")
	store := graphstore.New()
	store.Merge(g, nil)
	return store
}

func TestBacklinks(t *testing.T) {
	var seeded []string
	tools := newTools(t, &fetchtest.Client{}, graphHooks(linkedStore(), &seeded))

	got := tools.Backlinks(t.Context(), "/c.md")
	if got.IsError || !strings.Contains(got.Text, "Backlinks for mark://host/c.md (1):\n\n- [/a.md](mark://host/a.md)") {
		t.Errorf("Backlinks = %q", got.Text)
	}
	if len(seeded) != 1 || seeded[0] != "host:6309" {
		t.Errorf("seeded = %v, want the target's host seeded before the lookup", seeded)
	}
	none := tools.Backlinks(t.Context(), "/lonely.md")
	if none.IsError || !strings.HasSuffix(none.Text, "No backlinks found for mark://host/lonely.md\nRun mark_graph to populate the graph store.") {
		t.Errorf("no backlinks = %q", none.Text)
	}
}

func TestGraphToolsNeedAStore(t *testing.T) {
	tools := newTools(t, &fetchtest.Client{}, graphHooks(nil, nil))
	for name, got := range map[string]marktools.Result{
		"backlinks": tools.Backlinks(t.Context(), "/c.md"),
		"graph":     tools.Graph(t.Context(), marktools.GraphArgs{URL: "/c.md"}),
		"export":    tools.GraphExport(t.Context()),
		"publish":   tools.GraphPublish(t.Context(), marktools.GraphPublishArgs{URL: "/graph.md", ExpectedVersion: new(0)}),
	} {
		if !got.IsError || got.Text != "graph store not available" {
			t.Errorf("%s = %+v", name, got)
		}
	}
}

func TestGraphCrawlsFromTheIdentityAndClampsDepth(t *testing.T) {
	var seeded []string
	store := graphstore.New()
	tools := newTools(t, &fetchtest.Client{}, graphHooks(store, &seeded))
	got := tools.Graph(t.Context(), marktools.GraphArgs{URL: "/index.md", Depth: 99})
	if got.IsError || !strings.Contains(got.Text, "mark://host/index.md") {
		t.Fatalf("Graph = %+v", got)
	}
	if store.GetNode("mark://host/index.md") == nil || len(seeded) != 1 {
		t.Errorf("node stored = %v, seeded = %v", store.GetNode("mark://host/index.md") != nil, seeded)
	}
}

func TestGraphPublishNamesTheTargetByIdentity(t *testing.T) {
	backend := &fetchtest.Client{}
	var seeded []string
	tools := newTools(t, backend, graphHooks(linkedStore(), &seeded))

	got := tools.GraphPublish(t.Context(), marktools.GraphPublishArgs{URL: "/graph.md", ExpectedVersion: new(0), Retention: 20})
	// The identity, never the dial address: no :6309 (ADR 0005).
	if got.IsError || !strings.HasPrefix(got.Text, "Published graph (2 nodes, 1 edges) to mark://host/graph.md\n") {
		t.Errorf("GraphPublish = %q", got.Text)
	}
	sent := backend.PublishCalls[0]
	if sent.Token != "write-token" || sent.Metadata["agent"] != "test-agent" || sent.Metadata["retention"] != "20" || sent.Body != linkedStore().Export() {
		t.Errorf("sent = %+v", sent)
	}
	for name, tt := range map[string]struct {
		args marktools.GraphPublishArgs
		want string
	}{
		"missing version":    {marktools.GraphPublishArgs{URL: "/graph.md"}, "expected_version is required"},
		"negative version":   {marktools.GraphPublishArgs{URL: "/graph.md", ExpectedVersion: new(-1)}, "expected_version must be >= 0"},
		"negative retention": {marktools.GraphPublishArgs{URL: "/graph.md", ExpectedVersion: new(0), Retention: -1}, "retention must be >= 0 (0 keeps every version)"},
	} {
		if got := tools.GraphPublish(t.Context(), tt.args); !got.IsError || got.Text != tt.want {
			t.Errorf("%s: got %+v, want %q", name, got, tt.want)
		}
	}
}
