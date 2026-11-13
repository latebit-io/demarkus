package marktools_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// mapSeen is a surface's seen store: what this agent was already shown.
type mapSeen struct {
	mu   sync.Mutex
	docs map[string]fetchdedup.Doc
}

func (m *mapSeen) Lookup(_ context.Context, key string) (fetchdedup.Doc, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.docs[key]
	return d, ok
}

func (m *mapSeen) Record(_ context.Context, key string, d fetchdedup.Doc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.docs == nil {
		m.docs = map[string]fetchdedup.Doc{}
	}
	m.docs[key] = d
}

func docBackend(body, version string) *fetchtest.Client {
	return &fetchtest.Client{FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{
			Status: protocol.StatusOK, Body: body, Metadata: map[string]string{"version": version, "etag": "e" + version},
		}}, nil
	}}
}

func fetchArgs(url string) marktools.FetchArgs {
	return marktools.FetchArgs{URL: url}
}

func TestFetchDedupsThroughTheSeenStore(t *testing.T) {
	hooks := directHooks()
	seen := &mapSeen{}
	hooks.Seen = seen
	tools := newTools(t, docBackend("# Doc\n\nbody\n", "4"), hooks)

	first := tools.Fetch(t.Context(), fetchArgs("/doc.md"))
	if first.IsError || !strings.Contains(first.Text, "body") {
		t.Fatalf("first fetch = %+v", first)
	}
	if _, ok := seen.docs["host:6309/doc.md"]; !ok {
		t.Fatalf("seen keys = %v, want host+path", seen.docs)
	}
	second := tools.Fetch(t.Context(), fetchArgs("/doc.md"))
	if second.Text != fetchdedup.UnchangedNotice(fetchdedup.Doc{Version: "4", Etag: "e4"}) {
		t.Errorf("second fetch = %q, want the unchanged notice", second.Text)
	}
	forced := fetchArgs("/doc.md")
	forced.Force = true
	if got := tools.Fetch(t.Context(), forced); got.Text != first.Text {
		t.Errorf("forced fetch = %q, want the full body again", got.Text)
	}
	section := tools.Fetch(t.Context(), fetchArgs("/doc.md#doc"))
	if section.IsError || !strings.Contains(section.Text, "section: #doc") {
		t.Errorf("section fetch = %+v, want the slice whatever was seen", section)
	}
}

// A fetch reads the sources its document pins on this store and says which
// moved; the unchanged notice carries the line too, since only a source moved.
func TestFetchReportsDrift(t *testing.T) {
	backend := &fetchtest.Client{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		switch r.Path {
		case "/notes/r.md":
			return fetchtest.Head("# R\n\nbody\n", 2, map[string]string{"etag": "e2", "rel-based-on": "../adr/a.md/v3, /gone.md/v1, /adr/b.md/v2, mark://host:6309/adr/d.md/v9, /adr/c.md, mark://kb/x.md/v1, https://github.com/o/r/tree/abcdef0"}), nil
		case "/adr/a.md":
			return fetchtest.Head("# A\n", 5, nil), nil
		case "/adr/b.md":
			return fetchtest.Head("# B\n", 2, nil), nil
		case "/adr/d.md":
			return fetchtest.Head("# D\n", 4, nil), nil
		case "/plain.md":
			return fetchtest.Head("# P\n\nbody\n", 1, nil), nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}}
	hooks := directHooks()
	hooks.Seen = &mapSeen{}
	tools := newTools(t, backend, hooks)
	const line = "drift: /adr/a.md is at v5, past the pinned v3; /gone.md is not a live document here; /adr/d.md has no v9; it is at v4; may be stale\n"

	first := tools.Fetch(t.Context(), fetchArgs("/notes/r.md"))
	if first.IsError || !strings.Contains(first.Text, line) {
		t.Fatalf("first fetch lacks the drift line:\n%s", first.Text)
	}
	// Only versioned pins on this store are read: no unversioned, other-store or repository source.
	read := make([]string, 0, len(backend.FetchCalls))
	for _, r := range backend.FetchCalls {
		read = append(read, r.Path)
	}
	slices.Sort(read)
	if want := []string{"/adr/a.md", "/adr/b.md", "/adr/d.md", "/gone.md", "/notes/r.md"}; !slices.Equal(read, want) {
		t.Errorf("read %q, want %q", read, want)
	}
	if second := tools.Fetch(t.Context(), fetchArgs("/notes/r.md")); !strings.Contains(second.Text, "status: unchanged\nversion: 2\netag: e2\n"+line+"\nunchanged since v2") {
		t.Errorf("unchanged notice lacks the drift line in its header:\n%s", second.Text)
	}
	if section := tools.Fetch(t.Context(), fetchArgs("/notes/r.md#r")); !strings.Contains(section.Text, line) {
		t.Errorf("section fetch lacks the drift line:\n%s", section.Text)
	}
	if plain := tools.Fetch(t.Context(), fetchArgs("/plain.md")); plain.IsError || strings.Contains(plain.Text, "drift") {
		t.Errorf("unpinned document: %q", plain.Text)
	}
}

// A source that cannot be read is counted, never taken as current.
func TestFetchCountsUnjudgedSources(t *testing.T) {
	backend := &fetchtest.Client{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		switch r.Path {
		case "/doc.md":
			return fetchtest.Head("# D\n\nbody\n", 1, map[string]string{"rel-based-on": "/s1.md/v1, /s2.md/v1, /s3.md/v1, /s4.md/v1, /s5.md/v1, /s6.md/v1, /s7.md/v1"}), nil
		case "/s1.md":
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
		}
		return fetchtest.Head("# S\n", 1, nil), nil
	}}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), fetchArgs("/doc.md"))
	// Nothing moved, so no stale claim.
	if got.IsError || !strings.Contains(got.Text, "drift: 1 unreadable; 2 past the limit of 5\n") {
		t.Errorf("fetch = %q, want one refused source and two past the bound", got.Text)
	}
}

// A surface with no seen store, as the broker has for a call without a
// session, never claims a document is unchanged.
func TestFetchWithoutASeenStoreAlwaysAnswersInFull(t *testing.T) {
	tools := newTools(t, docBackend("# Doc\n\nbody\n", "4"), directHooks())
	first := tools.Fetch(t.Context(), fetchArgs("/doc.md"))
	second := tools.Fetch(t.Context(), fetchArgs("/doc.md"))
	if first.IsError || first.Text != second.Text || !strings.Contains(second.Text, "body") {
		t.Errorf("first = %q\nsecond = %q", first.Text, second.Text)
	}
}

func TestFetchOutlinesLargeDocumentsAndNamesMissingSections(t *testing.T) {
	large := "# Big\n\n## Part\n\n" + strings.Repeat("filler line\n", 1200)
	tools := newTools(t, docBackend(large, "1"), directHooks())
	outline := tools.Fetch(t.Context(), fetchArgs("mark-url/big.md"))
	if outline.IsError || !strings.Contains(outline.Text, "mode: outline") || !strings.Contains(outline.Text, "fetch mark-url/big.md#<anchor>") {
		t.Errorf("large fetch = %.300q", outline.Text)
	}
	missing := tools.Fetch(t.Context(), fetchArgs("/big.md#nope"))
	if !missing.IsError || missing.Text != "section #nope not found in /big.md; available anchors: big, part" {
		t.Errorf("missing section = %+v", missing)
	}
}

func TestReadResource(t *testing.T) {
	tools := newTools(t, docBackend("# Doc\n\n## Part\n\ntext\n", "1"), directHooks())
	whole, err := tools.ReadResource(t.Context(), "/doc.md")
	if err != nil || whole.MIMEType != "text/markdown" || !strings.Contains(whole.Text, "## Part") {
		t.Fatalf("whole = %+v, %v", whole, err)
	}
	part, err := tools.ReadResource(t.Context(), "/doc.md#part")
	if err != nil || strings.Contains(part.Text, "# Doc") || !strings.Contains(part.Text, "text") {
		t.Errorf("section = %+v, %v", part, err)
	}
	if _, err := tools.ReadResource(t.Context(), "/doc.md#nope"); err == nil || !strings.Contains(err.Error(), "available anchors: doc, part") {
		t.Errorf("missing section err = %v", err)
	}
	if _, err := tools.ReadResource(t.Context(), "bad"); err == nil || err.Error() != `invalid resource URI "bad": unsupported scheme` {
		t.Errorf("bad URI err = %v", err)
	}
}
