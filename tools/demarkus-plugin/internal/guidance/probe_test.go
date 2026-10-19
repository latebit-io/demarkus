package guidance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

// fakeSoul answers FETCH from docs (absent is not found) and LIST of the
// journal from entries; listErr fails the listing.
func fakeSoul(docs map[string]protocol.Response, journalDir string, entries []string, listErr error) soul {
	c := &fetchtest.Client{
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			if resp, ok := docs[r.Path]; ok {
				return fetch.Result{Response: resp}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if listErr != nil {
				return fetch.Result{}, listErr
			}
			if r.Path != journalDir {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
			}
			return fetchtest.ListPage(r.Path, "", entries...), nil
		},
	}
	return soul{reader: c, host: "soul.example:6309"}
}

func ok(body string) protocol.Response {
	return protocol.Response{Status: protocol.StatusOK, Body: body}
}

func TestProbeStates(t *testing.T) {
	journal := []string{"2026-09-30.md", "2026-10-02.md", "2026-10-13.md", "2026-99-01.md", "notes.md", "sub/"}
	tests := []struct {
		name    string
		docs    map[string]protocol.Response
		entries []string
		listErr error
		want    ProjectStatus
	}{
		{name: "known with newest entry", docs: map[string]protocol.Response{"/app/index.md": ok("# App")}, entries: journal,
			want: ProjectStatus{State: ProjectKnown, Root: "/app/", LastJournal: "2026-10-13", JournalPath: "/app/journal/2026-10-13.md"}},
		{name: "archived hub is known", docs: map[string]protocol.Response{"/app/index.md": {Status: protocol.StatusArchived}},
			want: ProjectStatus{State: ProjectKnown, Root: "/app/", NoJournal: true}},
		{name: "no hub is new", want: ProjectStatus{State: ProjectNew, Root: "/app/"}},
		{name: "dir-named hub is known", docs: map[string]protocol.Response{"/app.md": ok("# App")}, entries: journal,
			want: ProjectStatus{State: ProjectKnown, Root: "/app/", LastJournal: "2026-10-13", JournalPath: "/app/journal/2026-10-13.md"}},
		{name: "unauthorized dir-named hub is unknown", docs: map[string]protocol.Response{"/app.md": {Status: protocol.StatusUnauthorized}},
			want: ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "/app.md: " + protocol.StatusUnauthorized}},
		{name: "unauthorized is unknown", docs: map[string]protocol.Response{"/app/index.md": {Status: protocol.StatusUnauthorized}},
			want: ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "/app/index.md: " + protocol.StatusUnauthorized}},
		{name: "root project reads the root hub", docs: map[string]protocol.Response{layout.DocumentPath: ok("root_project: app\n"), "/index.md": ok("# Soul")}, entries: journal,
			want: ProjectStatus{State: ProjectKnown, Root: "/", LastJournal: "2026-10-13", JournalPath: "/journal/2026-10-13.md"}},
		{name: "root project without a root hub is new", docs: map[string]protocol.Response{layout.DocumentPath: ok("root_project: app\n"), "/app/index.md": ok("# App")},
			want: ProjectStatus{State: ProjectNew, Root: "/"}},
		{name: "sub-project keeps its own directory", docs: map[string]protocol.Response{layout.DocumentPath: ok("root_project: soul\n"), "/index.md": ok("# Soul"), "/app/index.md": ok("# App")}, entries: journal,
			want: ProjectStatus{State: ProjectKnown, Root: "/app/", LastJournal: "2026-10-13", JournalPath: "/app/journal/2026-10-13.md"}},
		{name: "invalid template is unknown even with a hub", docs: map[string]protocol.Response{layout.DocumentPath: ok("root_project: /app/\n"), "/app/index.md": ok("# App")},
			want: ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "template /.well-known/demarkus/template.md: invalid root_project"}},
		{name: "unreadable template is unknown", docs: map[string]protocol.Response{layout.DocumentPath: {Status: protocol.StatusUnauthorized}, "/app/index.md": ok("# App")},
			want: ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "fetch /.well-known/demarkus/template.md: " + protocol.StatusUnauthorized}},
		{name: "listing error keeps known", docs: map[string]protocol.Response{"/app/index.md": ok("# App")}, listErr: errors.New("reset"),
			want: ProjectStatus{State: ProjectKnown, Root: "/app/"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := "/app/journal/"
			if tt.want.Root == "/" {
				dir = "/journal/"
			}
			got := probe(t.Context(), fakeSoul(tt.docs, dir, tt.entries, tt.listErr), "app")
			if got != tt.want {
				t.Errorf("probe = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProbeTimeoutIsUnknownWithinDeadline(t *testing.T) {
	hang := &fetchtest.Client{FetchFn: func(ctx context.Context, _ fetch.FetchRequest) (fetch.Result, error) {
		<-ctx.Done()
		return fetch.Result{}, ctx.Err()
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := probe(ctx, soul{reader: hang, host: "soul.example:6309"}, "app")
	if time.Since(start) > time.Second {
		t.Errorf("probe outlived its deadline: %s", time.Since(start))
	}
	if got.State != ProjectUnknown || !strings.Contains(got.Reason, "no answer within") {
		t.Errorf("timeout = %+v", got)
	}
}

func TestProbeStoreSkipsBrokerStores(t *testing.T) {
	setupHome(t, map[string]string{"souls": "team\thttps://broker.example\tfalse\t-\n"})
	got := probeStore(t.Context(), "team", "app")
	if got.State != ProjectUnknown || got.Reason != "hosted store, not checked" || got.Root != "/app/" {
		t.Errorf("broker = %+v", got)
	}
	if got := probeStore(t.Context(), "gone", "app"); got.State != ProjectUnknown || !strings.Contains(got.Reason, "not in the catalog") {
		t.Errorf("stale = %+v", got)
	}
}

func TestVerdictStopsOnlyForAStaleBinding(t *testing.T) {
	known := ProjectStatus{State: ProjectKnown}
	unknown := ProjectStatus{State: ProjectUnknown, Reason: "hosted store, not checked"}
	stale := project.Resolution{Store: "gone", State: project.StateStale}
	for _, c := range []struct {
		r    project.Resolution
		st   ProjectStatus
		want string
	}{
		{project.Resolution{Store: "team", State: project.StateBound}, known, "use"},
		{project.Resolution{Store: "demarkus-memory", State: project.StateLocal}, unknown, "use, root unconfirmed: hosted store, not checked"},
		{stale, unknown, "stop: the bound store is " + stale.Hint()},
	} {
		if got := Verdict(c.r, c.st); got != c.want {
			t.Errorf("Verdict(%s, %s) = %q, want %q", c.r.State, c.st.State, got, c.want)
		}
	}
}

func TestProjectStatusLines(t *testing.T) {
	known := ProjectStatus{State: ProjectKnown, Root: "/"}
	unknown := ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "hosted store, not checked"}
	if known.Lines() != "root=/\nproject=known" || unknown.Lines() != "root=/app/\nproject=unknown\nreason=hosted store, not checked" {
		t.Errorf("lines = %q / %q", known.Lines(), unknown.Lines())
	}
}
