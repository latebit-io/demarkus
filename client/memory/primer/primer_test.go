package primer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/primer"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/layout"
)

func read(t *testing.T, backend *fetchtest.Client, dir string) primer.Primer {
	t.Helper()
	return primer.Read(t.Context(), &primer.Request{
		Dir: dir, Layout: layout.Default(),
		Fetch: func(ctx context.Context, p string) (protocol.Response, error) {
			r, err := backend.Fetch(ctx, fetch.FetchRequest{Host: "host:6309", Path: p})
			return r.Response, err
		},
		Walker: listwalk.Walker{Client: backend, Host: "host:6309"},
	})
}

func TestReadStates(t *testing.T) {
	if p := read(t, &fetchtest.Client{}, "/proj/"); p.State != primer.New || len(p.HubPaths) == 0 || len(p.Sections) != 0 {
		t.Errorf("no hub = %+v, want a new project naming where its hub may live", p)
	}
	retired := &fetchtest.Client{FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
		return fetchtest.Archived(), nil
	}}
	if p := read(t, retired, "/proj/"); p.State != primer.Archived || p.Hub == "" {
		t.Errorf("archived hub = %+v", p)
	}
}

func TestReadGathersHubJournalAndWork(t *testing.T) {
	l := layout.Default()
	long := "# Roadmap\n\n" + strings.Repeat("a line of the roadmap\n", 400)
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309/proj/index.md":                       fetchtest.Head("# Proj\n", 1, nil),
			"host:6309/proj/" + l.Roadmap:                   fetchtest.Head(long, 2, nil),
			"host:6309/proj/" + l.Journal + "2026-10-12.md": fetchtest.Head("# 12\n", 1, map[string]string{"tags": "journal"}),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			switch r.Path {
			case "/proj/" + l.Journal:
				return fetchtest.ListPage(r.Path, "", "2026-10-11.md", "2026-10-12.md", "notes.md"), nil
			case "/proj/" + l.Plans:
				return fetch.Result{}, errors.New("list refused")
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	p := read(t, backend, "/proj/")
	if p.State != primer.Known || len(p.Sections) != 2 || p.Sections[0].Heading != "Hub" || !strings.HasSuffix(p.Sections[1].Heading, " 2026-10-12") || p.Sections[1].Metadata["tags"] != "journal" {
		t.Fatalf("sections = %+v", p.Sections)
	}
	if p.Active == nil || !p.Active.Cut || len(p.Active.Body) > 4000 || strings.HasSuffix(p.Active.Body, "\n") {
		t.Errorf("active work = %+v, want it cut at a line under the cap", p.Active)
	}
	// The unread journal day and the refused plans listing are gaps, said so.
	coverage := strings.Join(p.Coverage, "\n")
	if !strings.Contains(coverage, "2026-10-11.md: not-found") || !strings.Contains(coverage, "list refused") || p.JournalEmpty {
		t.Errorf("coverage = %q, journalEmpty = %v", p.Coverage, p.JournalEmpty)
	}
}

func TestBound(t *testing.T) {
	for _, c := range []struct {
		body  string
		limit int
		want  string
		cut   bool
	}{
		{"short\n\n", 10, "short", false},
		{"line one\nline two\nline three\n", 20, "line one\nline two", true},
		{"ééééé", 5, "éé", true}, // no line to cut at: a rune boundary
	} {
		if got, cut := primer.Bound(c.body, c.limit); got != c.want || cut != c.cut {
			t.Errorf("Bound(%q, %d) = %q, %v; want %q, %v", c.body, c.limit, got, cut, c.want, c.cut)
		}
	}
}
