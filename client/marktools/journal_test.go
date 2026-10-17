package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
)

// The journal mechanic is tested in client/journal; here that the route
// authorizes once, writes through the surface, and words the outcome.
func TestAppendRouteTodayCreatesTheJournal(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, writingHooks())
	got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/proj/", Body: "Shipped it.", Route: "today", Tags: "release, auth"})
	if got.IsError || !strings.HasPrefix(got.Text, "status: created\nversion: 1\npath: /proj/journal/") || strings.Contains(got.Text, "note:") {
		t.Fatalf("append = %+v", got)
	}
	var journal *fetch.WriteRequest
	for i := range backend.PublishCalls {
		if strings.HasPrefix(backend.PublishCalls[i].Path, "/proj/journal/") {
			journal = &backend.PublishCalls[i]
		}
	}
	if journal == nil || journal.Token != "write-token" || journal.Metadata["tags"] != "journal,proj,release,auth" || journal.Metadata["agent"] != "test-agent" {
		t.Fatalf("journal publish = %+v", journal)
	}
	if len(backend.PublishCalls) != 3 {
		t.Errorf("publishes = %d, want journal plus two hubs", len(backend.PublishCalls))
	}
}

func TestAppendRouteIsCheckedBeforeAuthorization(t *testing.T) {
	backend := &fetchtest.Client{}
	tools := newTools(t, backend, writingHooks())
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/locked.md", Body: "x", Route: "tomorrow"}); !got.IsError || !strings.HasPrefix(got.Text, "invalid route") {
		t.Fatalf("bad route = %+v", got)
	}
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/locked.md", Body: "x", Route: "today"}); !got.IsError || got.Text != "append requires a token" {
		t.Fatalf("unauthorized = %+v", got)
	}
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/proj/", Body: " ", Route: "today"}); !got.IsError || got.Text != "append failed: entry is empty" {
		t.Fatalf("empty = %+v", got)
	}
	if got := tools.Publish(t.Context(), marktools.PublishArgs{URL: "/proj/.context", Body: "x", ExpectedVersion: new(0)}); !got.IsError || !strings.Contains(got.Text, "reserved") {
		t.Fatalf("publish to the primer = %+v", got)
	}
	if got := tools.Append(t.Context(), marktools.AppendArgs{URL: "/.context", Body: "x"}); !got.IsError || !strings.Contains(got.Text, "reserved") {
		t.Fatalf("append to the primer = %+v", got)
	}
	if len(backend.PublishCalls)+len(backend.AppendCalls) != 0 {
		t.Error("nothing may be written")
	}
}

func TestContextPrimerReadsHubJournalAndPlans(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309/proj/index.md":              fetchtest.Head("# Proj\n\nThe hub.\n", 1, nil),
			"host:6309/proj/journal/2026-10-10.md": fetchtest.Head("# day 10\n", 1, nil),
			"host:6309/proj/journal/2026-10-11.md": fetchtest.Head("# day 11\n", 1, nil),
			"host:6309/proj/journal/2026-10-12.md": fetchtest.Head("# day 12\n", 1, nil),
			"host:6309/proj/journal/2026-10-09.md": fetchtest.Head("# day 9\n", 1, nil),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			switch r.Path {
			case "/proj/journal/":
				return fetchtest.ListPage(r.Path, "", "2026-10-09.md", "2026-10-10.md", "2026-10-11.md", "2026-10-12.md", "README.md"), nil
			case "/proj/plans/":
				return fetchtest.ListPage(r.Path, "", "a.md", "b.md"), nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	tools := newTools(t, backend, directHooks())
	got := tools.Fetch(t.Context(), marktools.FetchArgs{URL: "/proj/.context"})
	if got.IsError {
		t.Fatalf("context = %+v", got)
	}
	for _, want := range []string{
		"# Context: /proj/\n", "## Hub (/proj/index.md)\n\n# Proj\n\nThe hub.\n",
		"## Journal 2026-10-12 (/proj/journal/2026-10-12.md)", "## Journal 2026-10-10 (", "## Plans\n\n- /proj/plans/a.md\n- /proj/plans/b.md\n",
	} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("primer missing %q:\n%s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "2026-10-09") || strings.Contains(got.Text, "README") || strings.Contains(got.Text, "Coverage") || strings.Contains(got.Text, "Active work") {
		t.Errorf("primer shows more than the newest three dated days, or a gap, or an absent roadmap:\n%s", got.Text)
	}
	resource, err := tools.ReadResource(t.Context(), "/proj/.context")
	if err != nil || resource.MIMEType != "text/markdown" || resource.Text != got.Text {
		t.Errorf("resource read differs from the fetch: %v %+v", err, resource)
	}
}

func TestContextPrimerFollowsTheTemplateLayout(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309/.well-known/demarkus/template.md": fetchtest.Head("journal: log\nroadmap: next.md\n", 1, nil),
			"host:6309/index.md":                         fetchtest.Head("# Hub\n", 1, nil),
			"host:6309/log/2026-10-12.md":                fetchtest.Head("# day\n", 1, nil),
			"host:6309/next.md":                          fetchtest.Head("# Next\n", 1, nil),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if r.Path == "/log/" {
				return fetchtest.ListPage(r.Path, "", "2026-10-12.md"), nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/.context"})
	if got.IsError || !strings.Contains(got.Text, "## Log 2026-10-12 (/log/2026-10-12.md)") || !strings.Contains(got.Text, "## Active work (/next.md)") {
		t.Fatalf("context = %+v", got)
	}
}

func TestContextPrimerNotesABadTemplate(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/.well-known/demarkus/template.md": fetchtest.Head("journal: Bad Dir\n", 1, nil),
		"host:6309/index.md":                         fetchtest.Head("# Hub\n", 1, nil),
	}}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/.context"})
	if got.IsError || !strings.Contains(got.Text, "invalid journal (default layout assumed)") {
		t.Fatalf("context = %+v", got)
	}
	// The route refuses to write under a layout it cannot trust.
	tools := newTools(t, backend, writingHooks())
	if w := tools.Append(t.Context(), marktools.AppendArgs{URL: "/", Body: "x", Route: "today"}); !w.IsError || !strings.Contains(w.Text, "invalid journal") {
		t.Fatalf("append = %+v", w)
	}
}

func TestContextPrimerReportsANewOrRetiredProject(t *testing.T) {
	backend := &fetchtest.Client{}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/proj/.context"})
	if got.IsError || !strings.HasPrefix(got.Text, "status: new\n\nNo hub at /proj/index.md or /proj.md:") || len(backend.ListCalls) != 0 {
		t.Fatalf("new project: %+v (lists %d)", got, len(backend.ListCalls))
	}
	backend = &fetchtest.Client{Published: map[string]fetch.Result{"host:6309/proj/index.md": {Response: protocol.Response{Status: protocol.StatusArchived}}}}
	got = newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/proj/.context"})
	if got.IsError || !strings.HasPrefix(got.Text, "status: archived\n\n/proj/index.md is archived") {
		t.Fatalf("retired project: %+v", got)
	}
}

func TestContextPrimerReadsADirNamedHub(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"host:6309/proj.md": fetchtest.Head("# Proj\n\n- [a](proj/a.md)\n", 2, nil)}}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/proj/.context"})
	if got.IsError || !strings.Contains(got.Text, "## Hub (/proj.md)\n\n# Proj") || strings.Contains(got.Text, "status: new") {
		t.Fatalf("context = %+v", got)
	}
}

func TestContextPrimerListsWhatItCouldNotRead(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/index.md": {Response: protocol.Response{Status: protocol.StatusUnauthorized}}},
	}
	got := newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/.context"})
	if got.IsError || !strings.Contains(got.Text, "## Journal\n\nNo entries yet.\n") || !strings.Contains(got.Text, "## Coverage\n\n- /index.md: unauthorized\n") {
		t.Fatalf("context = %+v", got)
	}

	// A journal that could not be listed is unknown, not empty.
	backend = &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/index.md": fetchtest.Head("# Hub\n", 1, nil)},
		ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
		},
	}
	got = newTools(t, backend, directHooks()).Fetch(t.Context(), marktools.FetchArgs{URL: "/.context"})
	if got.IsError || strings.Contains(got.Text, "No entries yet") || !strings.Contains(got.Text, "- /journal/: list /journal/: server returned \"unauthorized\"") {
		t.Fatalf("context = %+v", got)
	}
}
