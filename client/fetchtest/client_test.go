package fetchtest

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

func TestPublishedDocumentKeepsPublisherMetadata(t *testing.T) {
	c := &Client{}
	meta := map[string]string{"tags": "a,b", "type": "Note", "version": "99"}
	write := fetch.WriteRequest{Host: "h", Path: "/doc.md", Body: "# Doc\n", Token: "token", Metadata: meta}
	if _, err := c.Publish(t.Context(), write); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, err := c.Fetch(t.Context(), fetch.FetchRequest{Host: "h", Path: "/doc.md"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := map[string]string{"tags": "a,b", "type": "Note", "version": "1"}
	for key, value := range want {
		if got.Response.Metadata[key] != value {
			t.Errorf("metadata[%q] = %q, want %q", key, got.Response.Metadata[key], value)
		}
	}
	if got.Response.Metadata["content-hash"] == "" {
		t.Error("stored document has no content-hash")
	}
	meta["tags"] = "changed"
	again, err := c.Fetch(t.Context(), fetch.FetchRequest{Host: "h", Path: "/doc.md"})
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if again.Response.Metadata["tags"] != "a,b" {
		t.Error("stored metadata aliases the caller's map")
	}
	if c.PublishCalls[0].Metadata["tags"] != "a,b" {
		t.Error("recorded metadata aliases the caller's map")
	}
}

// A real client sends nothing for a context that is already done, so no
// scripted answer may run for one, whatever the verb.
func TestDoneContextRunsNoCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ran := func(name string) (fetch.Result, error) {
		t.Errorf("%s ran for a context that was already done", name)
		return fetch.Result{}, nil
	}
	c := &Client{
		FetchFn:    func(context.Context, fetch.FetchRequest) (fetch.Result, error) { return ran("FetchFn") },
		SeedFn:     func(context.Context, fetch.FetchRequest) (fetch.Result, error) { return ran("SeedFn") },
		ListFn:     func(context.Context, fetch.ListRequest) (fetch.Result, error) { return ran("ListFn") },
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) { return ran("VersionsFn") },
		LookupFn:   func(context.Context, fetch.LookupRequest) (fetch.Result, error) { return ran("LookupFn") },
		PublishFn:  func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return ran("PublishFn") },
		AppendFn:   func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return ran("AppendFn") },
		ArchiveFn:  func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) { return ran("ArchiveFn") },
	}
	calls := map[string]func() error{
		"Fetch": func() error { _, err := c.Fetch(ctx, fetch.FetchRequest{Host: "h", Path: "/doc.md"}); return err },
		"Seed": func() error {
			_, err := c.Fetch(ctx, fetch.FetchRequest{Host: "h", Path: graphstore.LegacyExportPath})
			return err
		},
		"List":     func() error { _, err := c.List(ctx, fetch.ListRequest{Host: "h", Path: "/"}); return err },
		"Versions": func() error { _, err := c.Versions(ctx, fetch.VersionsRequest{Host: "h", Path: "/doc.md"}); return err },
		"Lookup": func() error {
			_, err := c.Lookup(ctx, fetch.LookupRequest{Host: "h", Scope: "/", Query: "q"})
			return err
		},
		"Publish": func() error { _, err := c.Publish(ctx, fetch.WriteRequest{Host: "h", Path: "/doc.md"}); return err },
		"Append":  func() error { _, err := c.Append(ctx, fetch.WriteRequest{Host: "h", Path: "/doc.md"}); return err },
		"Archive": func() error { _, err := c.Archive(ctx, fetch.ArchiveRequest{Host: "h", Path: "/doc.md"}); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Errorf("%s err = %v, want context.Canceled", name, err)
		}
	}
	if recorded := c.Calls(); len(recorded.Fetch)+len(recorded.Seed)+len(recorded.Publish) != 0 {
		t.Errorf("calls recorded for a done context: %+v", recorded)
	}
}

// Seed documents have their own channel: FetchFn often answers any path, and
// a graph seeder must not parse a test's document body as a graph.
func TestSeedDocumentsNeverReachFetchFn(t *testing.T) {
	anyDoc := func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "# Doc"}}, nil
	}
	seed := func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "seed " + r.Path}}, nil
	}
	snapshot := func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "snapshot"}}, nil
	}
	fetchBody := func(c *Client, path string) string {
		t.Helper()
		got, err := c.Fetch(t.Context(), fetch.FetchRequest{Host: "h", Path: path, IfNoneMatch: "etag"})
		if err != nil {
			t.Fatalf("Fetch %s: %v", path, err)
		}
		return got.Response.Status + " " + got.Response.Body
	}

	unscripted := &Client{FetchFn: anyDoc}
	for _, path := range []string{graphstore.LegacyExportPath, graphstore.SnapshotManifestPath} {
		if got := fetchBody(unscripted, path); got != protocol.StatusNotFound+" " {
			t.Errorf("unscripted seed %s = %q, want not found", path, got)
		}
	}
	if got := fetchBody(unscripted, "/doc.md"); got != "ok # Doc" {
		t.Errorf("document = %q, want FetchFn's answer", got)
	}

	scripted := &Client{FetchFn: anyDoc, SeedFn: seed}
	if got := fetchBody(scripted, graphstore.SnapshotManifestPath); got != "ok seed "+graphstore.SnapshotManifestPath {
		t.Errorf("manifest = %q, want SeedFn's answer", got)
	}
	scripted.SnapshotFn = snapshot
	if got := fetchBody(scripted, graphstore.SnapshotManifestPath); got != "ok snapshot" {
		t.Errorf("manifest = %q, want SnapshotFn to win", got)
	}
	if got := fetchBody(scripted, graphstore.LegacyExportPath); got != "ok seed "+graphstore.LegacyExportPath {
		t.Errorf("legacy export = %q, want SeedFn's answer", got)
	}
	if len(scripted.SeedCalls) != 3 || len(scripted.FetchCalls) != 0 || scripted.SeedCalls[0].IfNoneMatch != "etag" {
		t.Errorf("seed calls = %+v, fetch calls = %+v", scripted.SeedCalls, scripted.FetchCalls)
	}

	published := &Client{}
	if _, err := published.Publish(t.Context(), fetch.WriteRequest{Host: "h", Path: graphstore.LegacyExportPath, Body: "graph"}); err != nil {
		t.Fatal(err)
	}
	if got := fetchBody(published, graphstore.LegacyExportPath); got != "ok graph" {
		t.Errorf("published seed = %q, want the stored body", got)
	}
}
