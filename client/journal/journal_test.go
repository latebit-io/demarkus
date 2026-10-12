package journal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

var day = time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)

const today = "/proj/journal/2026-10-12.md"

var stamp = map[string]string{"agent": "test-agent"}

func docsOn(backend *fetchtest.Client) Docs {
	return func(p string) *docwrite.Doc {
		return &docwrite.Doc{Backend: backend, Host: "h", Path: p, ReadToken: "r", Write: docwrite.SendOnce("w")}
	}
}

func call(backend *fetchtest.Client, path string) *fetch.WriteRequest {
	for i := range backend.PublishCalls {
		if backend.PublishCalls[i].Path == path {
			return &backend.PublishCalls[i]
		}
	}
	return nil
}

func TestAppendCreatesTheDayAndBothHubs(t *testing.T) {
	backend := &fetchtest.Client{}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "Shipped the thing.", Tags: []string{"release", " "}, Stamp: stamp, Now: day})
	if err != nil {
		t.Fatal(err)
	}
	if out.Path != today || out.Status != StatusCreated || out.Version != 1 || len(out.Notes) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	j := call(backend, today)
	if j == nil || j.Body != "# proj journal: 2026-10-12\n\nShipped the thing.\n" || j.ExpectedVersion != 0 || j.Token != "w" {
		t.Fatalf("journal publish = %+v", j)
	}
	if j.Metadata["tags"] != "journal,proj,release" || j.Metadata["type"] != "Journal" || j.Metadata["importance"] != "0.4" || j.Metadata["agent"] != "test-agent" {
		t.Errorf("journal metadata = %v", j.Metadata)
	}
	hub := call(backend, "/proj/index.md")
	if hub == nil || !strings.Contains(hub.Body, "# proj\n\n## Journal\n\n- [Journal](journal/)") || hub.Metadata["tags"] != "index,proj" || hub.Metadata["agent"] != "test-agent" {
		t.Errorf("hub publish = %+v", hub)
	}
	root := call(backend, "/index.md")
	if root == nil || root.Body != "# Projects\n\n- [proj](/proj/)\n" || root.Metadata["importance"] != "0.9" {
		t.Errorf("root publish = %+v", root)
	}
	if len(backend.AppendCalls) != 0 {
		t.Error("a created journal is not appended to")
	}
}

func TestAppendAddsTheEntryOnceWithWidenedTags(t *testing.T) {
	body := "# Demarkus journal: 2026-10-12\n\nFirst entry.\n"
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"h" + today:       fetchtest.Head(body, 1, map[string]string{"tags": "journal,proj", "type": "Journal", "retention": "5"}),
		"h/proj/index.md": fetchtest.Head("# Demarkus\n\n- [Journal](journal/): notes\n", 4, map[string]string{"tags": "index"}),
		"h/index.md":      fetchtest.Head("# Projects\n\n- [Demarkus](/proj/)\n", 2, nil),
	}}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "Second entry.", Tags: []string{"auth", "proj"}, Stamp: stamp, Now: day})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusAppended || out.Version != 2 || len(out.Notes) != 0 {
		t.Fatalf("outcome = %+v", out)
	}
	if len(backend.AppendCalls) != 1 {
		t.Fatalf("append calls = %+v", backend.AppendCalls)
	}
	a := backend.AppendCalls[0]
	if a.Body != "\nSecond entry.\n" || a.ExpectedVersion != 1 || a.Metadata["tags"] != "journal,proj,auth" || a.Metadata["agent"] != "test-agent" {
		t.Errorf("append = %+v", a)
	}
	if _, kept := a.Metadata["retention"]; kept {
		t.Error("retention must never be resent")
	}
	if len(backend.PublishCalls) != 0 {
		t.Errorf("hubs that already link the journal were republished: %+v", backend.PublishCalls)
	}

	// The same entry again is present, not written.
	backend.AppendCalls = nil
	backend.Published["h"+today] = fetchtest.Head(body+"\nSecond entry.\n", 2, map[string]string{"tags": "journal,proj"})
	out, err = Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "Second entry.", Now: day})
	if err != nil || out.Status != StatusPresent || out.Version != 2 || len(backend.AppendCalls) != 0 {
		t.Fatalf("present outcome = %+v err %v appends %d", out, err, len(backend.AppendCalls))
	}
}

func TestAppendRetriesOneConflict(t *testing.T) {
	body := "# proj journal: 2026-10-12\n\nFirst.\n"
	conflicts := 0
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"h" + today:       fetchtest.Head(body, 1, map[string]string{"tags": "journal,proj"}),
			"h/proj/index.md": fetchtest.Head("# proj\n\n- [Journal](journal/)\n", 1, nil),
			"h/index.md":      fetchtest.Head("# Projects\n\n- [proj](/proj/)\n", 1, nil),
		},
		AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			if conflicts == 0 {
				conflicts++
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "2"}}}, nil
		},
	}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "Auth fixed.", Now: day})
	if err != nil || out.Status != StatusAppended || out.Version != 2 || len(backend.AppendCalls) != 2 {
		t.Fatalf("outcome = %+v err %v appends %d", out, err, len(backend.AppendCalls))
	}
}

func TestAppendLinksAnExistingHubUnderItsJournalSection(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"h/proj/index.md": fetchtest.Head("# Demarkus\n\nThe hub.\n\n## Journal\n\n- [2026-10-11](journal/2026-10-11.md): yesterday\n\n## Plans\n\n- [p](plans/p.md)\n", 3, map[string]string{"tags": "index,proj", "importance": "0.8"}),
		"h/index.md":      fetchtest.Head("# Projects\n\n- [Other](/other/)\n", 1, map[string]string{"tags": "projects,index", "agent": "someone-else"}),
	}}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "Today.", Stamp: stamp, Now: day})
	if err != nil || out.Status != StatusCreated || len(out.Notes) != 0 {
		t.Fatalf("outcome = %+v err %v", out, err)
	}
	if j := call(backend, today); j == nil || !strings.HasPrefix(j.Body, "# Demarkus journal: 2026-10-12\n") {
		t.Errorf("journal title should come from the hub H1: %+v", j)
	}
	if hub := call(backend, "/proj/index.md"); hub != nil {
		t.Errorf("a hub linking a dated entry already reaches the journal: %+v", hub)
	}
	root := call(backend, "/index.md")
	if root == nil || root.Body != "# Projects\n\n- [Other](/other/)\n- [Demarkus](/proj/)\n" || root.ExpectedVersion != 1 || root.Metadata["tags"] != "projects,index" || root.Metadata["agent"] != "test-agent" {
		t.Errorf("root publish = %+v", root)
	}
	// The scope hub is read once: for its title and as the edit's head.
	reads := 0
	for _, r := range backend.FetchCalls {
		if r.Path == "/proj/index.md" {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("scope hub fetched %d times, want 1", reads)
	}
}

func TestAppendReportsHubFailuresAsNotes(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"h/proj/index.md": {Response: protocol.Response{Status: protocol.StatusUnauthorized}}},
	}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "x", Now: day})
	if err != nil || out.Status != StatusCreated {
		t.Fatalf("outcome = %+v err %v", out, err)
	}
	if len(out.Notes) != 1 || out.Notes[0] != "hub /proj/index.md not updated: unauthorized" {
		t.Errorf("notes = %q", out.Notes)
	}
}

func TestAppendRefusesWhatItCannotRead(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h" + today: {Response: protocol.Response{Status: protocol.StatusArchived}}}}
	if _, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "x", Now: day}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Append(t.Context(), docsOn(backend), Request{Dir: "/proj/", Entry: "  "}); err == nil {
		t.Fatal("empty entry accepted")
	}
	if len(backend.PublishCalls)+len(backend.AppendCalls) != 0 {
		t.Error("nothing may be written")
	}
}

func TestRootScopeHasNoProjectLink(t *testing.T) {
	backend := &fetchtest.Client{}
	out, err := Append(t.Context(), docsOn(backend), Request{Dir: "/", Entry: "Flat memory entry.", Now: day})
	if err != nil || out.Path != "/journal/2026-10-12.md" {
		t.Fatalf("outcome = %+v err %v", out, err)
	}
	if j := call(backend, "/journal/2026-10-12.md"); j == nil || !strings.HasPrefix(j.Body, "# Memory journal: 2026-10-12\n") || j.Metadata["tags"] != "journal" {
		t.Errorf("journal publish = %+v", j)
	}
	if hub := call(backend, "/index.md"); hub == nil || !strings.HasPrefix(hub.Body, "# Memory\n\n## Journal") || hub.Metadata["tags"] != "index" {
		t.Errorf("hub publish = %+v", hub)
	}
	if len(backend.PublishCalls) != 2 {
		t.Errorf("publishes = %d, want journal and hub only", len(backend.PublishCalls))
	}
}

func TestHasBlock(t *testing.T) {
	if !hasBlock("# T\n\nfirst\n\nsecond line\n", "second line") || hasBlock("# T\n\nfirst second line\n", "second line") {
		t.Error("hasBlock matches whole blocks only")
	}
}
