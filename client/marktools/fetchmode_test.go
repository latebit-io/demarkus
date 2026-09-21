package marktools_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchdedup"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
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
	return marktools.FetchArgs{URL: url, Render: mcpfmt.Options{Envelope: &mcpfmt.Fetch}}
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
	if second.Text != fetchdedup.UnchangedNotice(fetchdedup.Doc{Version: "4", Etag: "e4"}, false) {
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
