package marktools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

const noteBody = "# QUIC retries\n\nBack off from 200ms, capped at 5s.\n\n## Detail\n\nMore.\n"

func publishNote(t *testing.T, backend *fetchtest.Client, url, body string, version int) marktools.Result {
	t.Helper()
	return newTools(t, backend, writingHooks()).Publish(t.Context(), marktools.PublishArgs{
		URL: url, Body: body, ExpectedVersion: new(version), OnConflict: "fail", Metadata: map[string]string{"tags": "quic"},
	})
}

// A first version is linked from the nearest hub that exists: its title,
// then the summary under the H1, under the surface's identity.
func TestPublishLinksAFirstVersionFromTheNearestHub(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/index.md":       fetchtest.Head("# Store\n\n- [Other](/other.md)\n", 3, map[string]string{"tags": "index"}),
		"host:6309/notes/index.md": fetchtest.Head("# Notes\n\nWhat is here.\n", 2, map[string]string{"tags": "notes,index"}),
	}}
	got := publishNote(t, backend, "/notes/quic.md", noteBody, 0)
	if got.IsError || got.Text != "status: created v1\nversion: 1\netag: etag-v1\nhubs:\n- /notes/index.md: bullet added\n" {
		t.Fatalf("publish = %+v", got)
	}
	hub := backend.PublishCall("/notes/index.md")
	if hub == nil || hub.Body != "# Notes\n\nWhat is here.\n\n- [QUIC retries](/notes/quic.md): Back off from 200ms, capped at 5s.\n" || hub.ExpectedVersion != 2 {
		t.Fatalf("hub publish = %+v", hub)
	}
	if hub.Metadata["tags"] != "notes,index" || hub.Metadata["agent"] != "test-agent" || hub.Metadata["user"] != "alice@example.com" {
		t.Errorf("hub metadata = %v", hub.Metadata)
	}
	if backend.PublishCall("/index.md") != nil {
		t.Error("the root hub was edited although a nearer hub exists")
	}
}

// Without an index.md, a directory's hub is the document named after it,
// where curate's split puts the topics.
func TestPublishLinksFromTheDocumentNamedAfterTheDirectory(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/index.md": fetchtest.Head("# Store\n\n- [Notes](/notes.md)\n", 3, nil),
		"host:6309/notes.md": fetchtest.Head("# Notes\n\nWhat is here.\n\n- [Old](/notes/old.md)\n", 4, nil),
	}}
	got := publishNote(t, backend, "/notes/quic.md", noteBody, 0)
	if got.IsError || !strings.HasSuffix(got.Text, "hubs:\n- /notes.md: bullet added\n") {
		t.Fatalf("publish = %+v", got)
	}
	if hub := backend.PublishCall("/notes.md"); hub == nil || !strings.HasSuffix(hub.Body, "- [Old](/notes/old.md)\n- [QUIC retries](/notes/quic.md): Back off from 200ms, capped at 5s.\n") {
		t.Errorf("hub publish = %+v", hub)
	}
}

// A hub that already names the document, by name or by a link to its
// directory, is left alone; a sibling's link is neither.
func TestPublishSkipsAHubThatNamesTheDocument(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/index.md": fetchtest.Head("# Store\n\n- [Notes](/notes/)\n", 3, nil),
	}}
	got := publishNote(t, backend, "/notes/quic.md", noteBody, 0)
	if got.IsError || got.Text != "status: created v1\nversion: 1\netag: etag-v1\n" || len(backend.PublishCalls) != 1 {
		t.Errorf("reached by directory = %+v, publishes %d", got, len(backend.PublishCalls))
	}
	// A sibling's link is not the directory's: the new document is still unnamed.
	backend = &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/index.md": fetchtest.Head("# Store\n\n- [Sibling](/notes/sibling.md)\n", 3, nil),
	}}
	if got := publishNote(t, backend, "/notes/quic.md", noteBody, 0); got.IsError || !strings.Contains(got.Text, "hubs:\n- /index.md: bullet added\n") {
		t.Errorf("sibling linked = %+v", got)
	}
}

// The bullet carries the description over the body's summary, and neither
// when it would put a status marker on the hub; a body without an H1 is
// named by its path.
func TestPublishBulletText(t *testing.T) {
	for name, tt := range map[string]struct {
		body, description, want string
	}{
		"description wins": {noteBody, "Retry timing for QUIC streams.", "- [QUIC retries](/notes/quic.md): Retry timing for QUIC streams."},
		"dated summary":    {"# Release\n\nShipped 2026-10-15 in #592.\n", "", "- [Release](/notes/quic.md)"},
		"no H1":            {"plain text\n", "", "- [quic](/notes/quic.md)"},
		"bracket in title": {"# API ](v2)\n\nThe v2 surface.\n", "", "- [API \\](v2)](/notes/quic.md): The v2 surface."},
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fetchtest.Client{Published: map[string]fetch.Result{"host:6309/index.md": fetchtest.Head("# Store\n", 1, nil)}}
			meta := map[string]string{"tags": "quic"}
			if tt.description != "" {
				meta["description"] = tt.description
			}
			got := newTools(t, backend, writingHooks()).Publish(t.Context(), marktools.PublishArgs{URL: "/notes/quic.md", Body: tt.body, ExpectedVersion: new(0), Metadata: meta})
			if got.IsError {
				t.Fatalf("publish = %+v", got)
			}
			if hub := backend.PublishCall("/index.md"); hub == nil || !strings.HasSuffix(hub.Body, "\n"+tt.want+"\n") {
				t.Errorf("hub = %+v, want a bullet %q", hub, tt.want)
			}
		})
	}
}

// Only a first version of a linkable document is linked: an update, a hub,
// a .well-known page and a dated journal entry read no hub at all.
func TestPublishLinksOnlyNewLinkableDocuments(t *testing.T) {
	for name, tt := range map[string]struct {
		url     string
		version int
	}{
		"update":        {"/notes/quic.md", 4},
		"hub":           {"/notes/index.md", 0},
		"well-known":    {"/.well-known/demarkus/template.md", 0},
		"journal entry": {"/journal/2026-10-15.md", 0},
	} {
		t.Run(name, func(t *testing.T) {
			backend := &fetchtest.Client{PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
				return fetchtest.Created(r.ExpectedVersion + 1), nil
			}}
			got := publishNote(t, backend, tt.url, "# Doc\n\nA summary.\n", tt.version)
			if got.IsError || strings.Contains(got.Text, "hubs:") || strings.Contains(got.Text, "note:") || len(backend.FetchCalls) != 0 {
				t.Errorf("publish = %+v, fetches %+v", got, backend.FetchCalls)
			}
		})
	}
}

// A hub that cannot be edited, or no hub at all, is a note after a publish
// that stands.
func TestPublishReportsAHubItCouldNotLink(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/index.md": fetchtest.Head("# Store\n", 1, nil)},
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			if r.Path == "/index.md" {
				return fetch.Result{}, errors.New("server unreachable")
			}
			return fetchtest.Created(1), nil
		},
	}
	got := publishNote(t, backend, "/quic.md", noteBody, 0)
	if got.IsError || got.Text != "status: created v1\nversion: 1\netag: etag-v1\n\nnote: hub /index.md not updated: server unreachable\n" {
		t.Errorf("hub failed = %+v", got)
	}

	backend = &fetchtest.Client{}
	got = publishNote(t, backend, "/notes/quic.md", noteBody, 0)
	if got.IsError || got.Text != "status: created v1\nversion: 1\netag: etag-v1\n\nnote: unlinked: no hub reaches /notes/quic.md; publish /notes/index.md with a bullet for it, or add one to /index.md\n" {
		t.Errorf("no hub = %+v", got)
	}

	// A hub that cannot be read is not absent: the walk stops and says so.
	backend = &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/notes/index.md": {Response: protocol.Response{Status: protocol.StatusUnauthorized}},
		"host:6309/index.md":       fetchtest.Head("# Store\n", 1, nil),
	}}
	got = publishNote(t, backend, "/notes/quic.md", noteBody, 0)
	if got.IsError || got.Text != "status: created v1\nversion: 1\netag: etag-v1\n\nnote: hub /notes/index.md not updated: unauthorized\n" || len(backend.PublishCalls) != 1 {
		t.Errorf("unreadable hub = %+v, publishes %d", got, len(backend.PublishCalls))
	}
	if len(backend.PublishCalls) != 1 {
		t.Errorf("publishes = %d, want the document alone", len(backend.PublishCalls))
	}
}

// The status line carries the version delta, so "created" on an update is
// not read as a new document; an append reads the same way.
func TestWriteStatusNamesTheVersions(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/doc.md": fetchtest.Head("# Doc\n\nOld.\n", 8, nil)},
		PublishFn: func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			return fetchtest.Created(r.ExpectedVersion + 1), nil
		},
	}
	got := publishNote(t, backend, "/doc.md", "# Doc\n\nNew.\n", 8)
	if got.IsError || !strings.HasPrefix(got.Text, "status: created v9 (was v8)\nversion: 9\n") {
		t.Errorf("update = %+v", got)
	}
	backend = &fetchtest.Client{AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetchtest.Created(3), nil
	}}
	got = newTools(t, backend, writingHooks()).Append(t.Context(), marktools.AppendArgs{URL: "/doc.md", Body: "more", ExpectedVersion: 2})
	if got.IsError || !strings.HasPrefix(got.Text, "status: created v3 (was v2)\n") {
		t.Errorf("append = %+v", got)
	}
}
