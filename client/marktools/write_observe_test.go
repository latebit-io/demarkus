package marktools_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

const observedBody = "# Doc\n\nSee the [hub](/hub.md).\n"

// A landed publish is observed like a read: node, body links and rel-* edges
// are in the graph before any crawl or explore.
func TestPublishObservesTheWrittenDocument(t *testing.T) {
	store := graphstore.New()
	var seeded []string
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, graphHooks(store, &seeded))
	got := tools.Publish(t.Context(), marktools.PublishArgs{
		URL: "/doc.md", Body: observedBody, ExpectedVersion: new(0), OnConflict: "fail",
		Metadata: map[string]any{"tags": "a", "rel-related": "/adr/0001.md"},
	})
	if got.IsError {
		t.Fatalf("Publish = %+v", got)
	}
	node := store.GetNode("mark://host/doc.md")
	if node == nil || node.Title != "Doc" || node.Status != protocol.StatusOK || node.Observation.Revision != 1 || !node.Observation.Complete {
		t.Fatalf("node = %+v, want a complete v1 observation titled Doc", node)
	}
	for _, target := range []string{"mark://host/hub.md", "mark://host/adr/0001.md"} {
		if back := store.Backlinks(target); !slices.Contains(back, "mark://host/doc.md") {
			t.Errorf("backlinks(%s) = %v, want the published document", target, back)
		}
	}
	if len(backend.FetchCalls) != 0 {
		t.Errorf("publish fetched %d documents to observe itself; the body was in hand", len(backend.FetchCalls))
	}
	if len(seeded) != 0 {
		t.Errorf("publish seeded the store: %v", seeded)
	}
}

// Observation records where the document really lives when the surface routes
// by alias, exactly as explore does.
func TestPublishObservesUnderTheScopesSource(t *testing.T) {
	store := graphstore.New()
	var seeded []string
	hooks := graphHooks(store, &seeded)
	hooks.Resolve = func(_ context.Context, raw string) (marktools.Target, error) {
		return marktools.Target{Host: "team-a", Path: raw, NodeURL: "mark://team-a" + raw}, nil
	}
	graphOf := hooks.Graph
	hooks.Graph = func(ctx context.Context) (*marktools.GraphScope, error) {
		scope, err := graphOf(ctx)
		scope.Source = func(target marktools.Target) string { return "mark://" + target.Host + ".svc:6309" + target.Path }
		return scope, err
	}
	got := newTools(t, &fetchtest.Client{}, hooks).Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: observedBody, ExpectedVersion: new(0)})
	if got.IsError {
		t.Fatalf("Publish = %+v", got)
	}
	node := store.GetNode("mark://team-a/doc.md")
	if node == nil || node.Observation.Source != "mark://team-a.svc/doc.md" {
		t.Errorf("node = %+v, want the observation sourced at the real address", node)
	}
}

// Without a graph scope the write is unchanged and nothing is fetched; a write
// that did not land is never observed.
func TestWritesWithoutAScopeOrWithoutLandingObserveNothing(t *testing.T) {
	backend := &fetchtest.Client{}
	got := newTools(t, backend, writingHooks()).Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: observedBody, ExpectedVersion: new(0)})
	if got.IsError || len(backend.FetchCalls) != 0 {
		t.Fatalf("Publish without a scope = %+v, fetches %d", got, len(backend.FetchCalls))
	}

	store := graphstore.New()
	var seeded []string
	backend = &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{}, errors.New("server unreachable")
	}}
	got = newTools(t, backend, graphHooks(store, &seeded)).Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: observedBody, ExpectedVersion: new(0)})
	if !got.IsError || store.GetNode("mark://host/doc.md") != nil {
		t.Fatalf("failed publish = %+v, node = %+v", got, store.GetNode("mark://host/doc.md"))
	}
}

// An append observes the new head with one read, since the client never holds
// the full appended body.
func TestAppendObservesTheNewHead(t *testing.T) {
	store := graphstore.New()
	var seeded []string
	head := fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: observedBody + "\nAlso [notes](/notes.md).\n", Metadata: map[string]string{"version": "2", "etag": "e2"}}}
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"host:6309" + protocol.VersionPath("/doc.md", 2): head}}
	got := newTools(t, backend, graphHooks(store, &seeded)).Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "\nAlso [notes](/notes.md).\n", ExpectedVersion: 1})
	if got.IsError {
		t.Fatalf("Append = %+v", got)
	}
	if len(backend.FetchCalls) != 1 || backend.FetchCalls[0].Path != protocol.VersionPath("/doc.md", 2) {
		t.Fatalf("fetches = %+v, want one read of v2", backend.FetchCalls)
	}
	node := store.GetNode("mark://host/doc.md")
	if node == nil || node.Observation.Revision != 2 || !node.Observation.Complete {
		t.Fatalf("node = %+v, want a complete v2 observation", node)
	}
	for _, target := range []string{"mark://host/hub.md", "mark://host/notes.md"} {
		if back := store.Backlinks(target); !slices.Contains(back, "mark://host/doc.md") {
			t.Errorf("backlinks(%s) = %v, want the appended document", target, back)
		}
	}
}

// A head the append cannot read back is a warning, never a failed append.
func TestAppendObservationFailureOnlyWarns(t *testing.T) {
	store := graphstore.New()
	var seeded, warnings []string
	hooks := graphHooks(store, &seeded)
	hooks.Warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	got := newTools(t, &fetchtest.Client{}, hooks).Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "x", ExpectedVersion: 1})
	if got.IsError {
		t.Fatalf("Append = %+v", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "after append mark://host:6309/doc.md") {
		t.Fatalf("warnings = %q, want one about the unread head", warnings)
	}
	if store.GetNode("mark://host/doc.md") != nil {
		t.Error("an unread head was observed")
	}
}

// Archiving is a confirmed observation: the node is archived and its stale
// outgoing edges are gone.
func TestArchiveObservesAbsenceAndClearsEdges(t *testing.T) {
	store := graphstore.New()
	var seeded []string
	tools := newTools(t, &fetchtest.Client{}, graphHooks(store, &seeded))
	if got := tools.Publish(t.Context(), marktools.PublishArgs{URL: "/doc.md", Body: observedBody, ExpectedVersion: new(0)}); got.IsError {
		t.Fatalf("Publish = %+v", got)
	}
	if got := tools.Archive(t.Context(), "/doc.md"); got.IsError {
		t.Fatalf("Archive = %+v", got)
	}
	node := store.GetNode("mark://host/doc.md")
	if node == nil || node.Status != protocol.StatusArchived || !node.Observation.Complete {
		t.Fatalf("node = %+v, want a complete archived observation", node)
	}
	if back := store.Backlinks("mark://host/hub.md"); slices.Contains(back, "mark://host/doc.md") {
		t.Errorf("backlinks(hub) = %v still lists the archived document", back)
	}
}
