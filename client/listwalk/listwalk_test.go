package listwalk

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

type stubLister struct {
	listings map[string][]string // dir -> entry names, trailing slash for a directory
	statuses map[string]string   // dir -> non-OK status
	pages    map[string]fetch.Result
	failures map[string]error // dir -> transport error
	requests []fetch.ListRequest
}

func (s *stubLister) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	if err := ctx.Err(); err != nil {
		return fetch.Result{}, err
	}
	s.requests = append(s.requests, r)
	if err, ok := s.failures[r.Path]; ok {
		return fetch.Result{}, err
	}
	dir := r.Path
	if page, ok := s.pages[dir+"\x00"+r.Cursor]; ok {
		return page, nil
	}
	if status, ok := s.statuses[dir]; ok {
		return fetch.Result{Response: protocol.Response{Status: status}}, nil
	}
	names, ok := s.listings[dir]
	if !ok {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	return fetchtest.ListPage(dir, "", names...), nil
}

func TestWalk(t *testing.T) {
	t.Run("collects files across subdirectories", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/":    {"a.md", "sub/"},
			"/sub": {"b.md"},
		}}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk(t.Context(), "/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		want := []string{"/a.md", "/sub/b.md"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("collects every page before completing directory", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00":     fetchtest.ListPage("/", "next", "a.md", "b.md"),
			"/\x00next": fetchtest.ListPage("/", "", "c.md"),
		}}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk(t.Context(), "/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		want := []string{"/a.md", "/b.md", "/c.md"}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("missing completeness is inconclusive", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00": {Response: protocol.Response{
				Status:   protocol.StatusOK,
				Metadata: map[string]string{"entries": "1"},
				Body:     "- [a.md](a.md)\n",
			}},
		}}
		err := (&Walker{Client: l, Host: "h", OnProblem: Skip(nil)}).Walk(t.Context(), "/", func(string) error { return nil })
		if !errors.Is(err, fetch.ErrListCompletenessUnknown) {
			t.Fatalf("error = %v, want ErrListCompletenessUnknown", err)
		}
	})

	t.Run("page budget counts continuations", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00": fetchtest.ListPage("/", "next", "a.md"),
		}}
		err := (&Walker{Client: l, Host: "h", MaxLists: 1, OnProblem: Skip(nil)}).Walk(t.Context(), "/", func(string) error { return nil })
		if !errors.Is(err, ErrListBudget) {
			t.Fatalf("error = %v, want ErrListBudget", err)
		}
	})

	t.Run("rejects cross-page ordering drift", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00":     fetchtest.ListPage("/", "next", "b.md"),
			"/\x00next": fetchtest.ListPage("/", "", "a.md"),
		}}
		if err := (&Walker{Client: l, Host: "h", OnProblem: Skip(nil)}).Walk(t.Context(), "/", func(string) error { return nil }); err == nil {
			t.Fatal("ordering drift accepted")
		}
	})

	t.Run("self-referencing listing terminates at MaxDepth", func(t *testing.T) {
		// A hostile server serving a self-listing at every depth mints
		// ever-deeper distinct paths; only MaxDepth stops it.
		listings := map[string][]string{"/": {"a.md", "loop/"}}
		dir := "/loop"
		for range 6 {
			listings[dir] = []string{"b.md", "loop/"}
			dir += "/loop"
		}
		l := &stubLister{listings: listings}
		var got []string
		w := Walker{Client: l, Host: "h", MaxDepth: 3, OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		// a.md + b.md at depths 1..3.
		if len(got) != 4 {
			t.Errorf("got %d files, want 4 (%v)", len(got), got)
		}
	})

	t.Run("strict walk reports depth truncation", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/":         {"sub/"},
			"/sub":      {"deep/"},
			"/sub/deep": {"x.md"},
		}}
		err := (&Walker{Client: l, Host: "h", MaxDepth: 1}).Walk(t.Context(), "/", func(string) error { return nil })
		if !errors.Is(err, ErrDepthBudget) {
			t.Fatalf("error = %v, want ErrDepthBudget", err)
		}
	})

	t.Run("list budget returns ErrListBudget", func(t *testing.T) {
		listings := map[string][]string{"/": {"d0/"}}
		dir := "/d0"
		for i := range 6 {
			listings[dir] = []string{fmt.Sprintf("d%d/", i+1)}
			dir += fmt.Sprintf("/d%d", i+1)
		}
		l := &stubLister{listings: listings}
		w := Walker{Client: l, Host: "h", MaxLists: 3, OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/", func(string) error { return nil }); !errors.Is(err, ErrListBudget) {
			t.Fatalf("got %v, want ErrListBudget", err)
		}
	})

	t.Run("OnSkip observes lenient skips", func(t *testing.T) {
		l := &stubLister{
			listings: map[string][]string{"/": {"a.md", "/etc/passwd", "sub/"}},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		var skips []string
		w := Walker{Client: l, Host: "h", OnProblem: Skip(func(p, reason string) { skips = append(skips, p+": "+reason) })}
		if err := w.Walk(t.Context(), "/", func(string) error { return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(skips) != 2 {
			t.Errorf("skips = %v, want unauthorized listing + invalid entry", skips)
		}
	})

	t.Run("escaped entry names are decoded", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/": {"my doc.md"},
		}}
		var got []string
		w := Walker{Client: l, Host: "h", OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/my doc.md" {
			t.Errorf("got %v, want [/my doc.md]", got)
		}
	})

	t.Run("strict errors on non-OK listing", func(t *testing.T) {
		l := &stubLister{
			listings: map[string][]string{"/": {"sub/"}},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk(t.Context(), "/", func(string) error { return nil }); err == nil {
			t.Fatal("expected error for non-OK listing in strict mode")
		}
	})

	t.Run("lenient skips non-OK listing", func(t *testing.T) {
		l := &stubLister{
			listings: map[string][]string{"/": {"a.md", "sub/"}},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		var got []string
		w := Walker{Client: l, Host: "h", OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/a.md" {
			t.Errorf("got %v, want [/a.md]", got)
		}
	})

	t.Run("escaping entries are skipped", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/docs": {"ok.md", "../secret.md", "/etc/passwd", "mark://evil.com/x.md"},
		}}
		var got []string
		w := Walker{Client: l, Host: "h", OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/docs", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/docs/ok.md" {
			t.Errorf("got %v, want [/docs/ok.md]", got)
		}
	})

	t.Run("strict errors on escaping entry", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/docs": {"../secret.md"},
		}}
		w := Walker{Client: l, Host: "h"}
		err := w.Walk(t.Context(), "/docs", func(string) error {
			t.Error("visit must not run for an invalid entry in strict mode")
			return nil
		})
		if err == nil {
			t.Fatal("expected error for escaping entry in strict mode")
		}
	})

	t.Run("visit error aborts", func(t *testing.T) {
		l := &stubLister{listings: map[string][]string{
			"/": {"a.md", "b.md"},
		}}
		sentinel := errors.New("stop")
		w := Walker{Client: l, Host: "h", OnProblem: Skip(nil)}
		if err := w.Walk(t.Context(), "/", func(string) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want sentinel", err)
		}
	})
}

// A cancelled walk stops at the next LIST and says why, instead of running
// to the list budget on a request nobody is waiting for.
func TestWalkStopsWhenContextIsDone(t *testing.T) {
	l := &stubLister{listings: map[string][]string{
		"/":    {"a.md", "sub/"},
		"/sub": {"b.md"},
	}}
	ctx, cancel := context.WithCancel(t.Context())
	var visited []string
	err := (&Walker{Client: l, Host: "h", OnProblem: Skip(nil)}).Walk(ctx, "/", func(p string) error {
		visited = append(visited, p)
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk err = %v, want context.Canceled", err)
	}
	if len(visited) != 1 || visited[0] != "/a.md" {
		t.Errorf("visited = %v, want the walk to stop after /a.md", visited)
	}
}

// A crawler's policy: whatever is wrong with one directory, note it and carry
// on with its siblings. Skip would abort on the failed LIST.
func TestWalkPolicyDecidesEveryProblem(t *testing.T) {
	l := &stubLister{
		listings: map[string][]string{
			"/":            {"a.md", "/etc/passwd", "broken/", "deep/", "denied/", "z.md"},
			"/deep":        {"deeper/"},
			"/deep/deeper": {"x.md"},
		},
		statuses: map[string]string{"/denied": protocol.StatusUnauthorized},
		failures: map[string]error{"/broken": errors.New("connection reset")},
	}
	var kinds []ProblemKind
	var visited []string
	w := Walker{Client: l, Host: "h", MaxDepth: 1, OnProblem: func(p *Problem) error {
		kinds = append(kinds, p.Kind)
		return nil
	}}
	if err := w.Walk(t.Context(), "/", func(p string) error { visited = append(visited, p); return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	wantKinds := []ProblemKind{ProblemInvalidEntry, ProblemList, ProblemDepth, ProblemStatus}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Errorf("problems = %v, want %v", kinds, wantKinds)
	}
	if fmt.Sprint(visited) != "[/a.md /z.md]" {
		t.Errorf("visited = %v, want the siblings of every problem", visited)
	}
}

func TestSkipStillAbortsOnAFailedList(t *testing.T) {
	cause := errors.New("connection reset")
	l := &stubLister{
		listings: map[string][]string{"/": {"broken/", "z.md"}},
		failures: map[string]error{"/broken": cause},
	}
	err := (&Walker{Client: l, Host: "h", OnProblem: Skip(nil)}).Walk(t.Context(), "/", func(string) error { return nil })
	var problem *Problem
	if !errors.As(err, &problem) || problem.Kind != ProblemList || problem.Dir != "/broken" || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want a ProblemList for /broken wrapping the cause", err)
	}
}

func TestWalkRunsBeforeListAndPassesOptions(t *testing.T) {
	l := &stubLister{listings: map[string][]string{"/": {"sub/"}, "/sub": {"b.md"}}}
	var pauses int
	w := Walker{Client: l, Host: "h", Token: "tok", IncludeArchived: true, BeforeList: func(context.Context) error {
		pauses++
		return nil
	}}
	if err := w.Walk(t.Context(), "/", func(string) error { return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if pauses != 2 || len(l.requests) != 2 {
		t.Fatalf("pauses = %d, lists = %d, want one pause before each of two LISTs", pauses, len(l.requests))
	}
	for _, r := range l.requests {
		if !r.IncludeArchived || r.Token != "tok" || r.Host != "h" || r.PageSize != protocol.MaxListPageSize {
			t.Errorf("request = %+v, want archived listing with the walker's host and token", r)
		}
	}
	if w.MaxDepth != 0 || w.MaxLists != 0 || w.OnProblem != nil {
		t.Errorf("Walk changed its Walker: %+v", w)
	}

	stop := errors.New("stop")
	w.BeforeList = func(context.Context) error { return stop }
	if err := w.Walk(t.Context(), "/", func(string) error { return nil }); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want BeforeList's error to end the walk", err)
	}
}

// Zero MaxDepth is the default, so "the root directory only" needs a name.
func TestWalkRootOnly(t *testing.T) {
	l := &stubLister{listings: map[string][]string{"/": {"a.md", "sub/"}, "/sub": {"b.md"}}}
	var visited []string
	var problems []string
	w := Walker{Client: l, Host: "h", MaxDepth: RootOnly, OnProblem: Skip(func(p, reason string) {
		problems = append(problems, p+": "+reason)
	})}
	if err := w.Walk(t.Context(), "/", func(p string) error { visited = append(visited, p); return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if fmt.Sprint(visited) != "[/a.md]" || fmt.Sprint(problems) != "[/sub: max depth reached]" {
		t.Errorf("visited = %v, problems = %v", visited, problems)
	}
}

// An audit needs the directories as well as the documents, empty ones included.
func TestWalkReportsEveryDirectoryItLists(t *testing.T) {
	l := &stubLister{listings: map[string][]string{
		"/":         {"a.md", "empty/", "sub/"},
		"/empty":    {},
		"/sub":      {"b.md", "deep/"},
		"/sub/deep": {"c.md"},
	}}
	var dirs []string
	w := Walker{Client: l, Host: "h", OnDir: func(dir string) { dirs = append(dirs, dir) }}
	if err := w.Walk(t.Context(), "/", func(string) error { return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if fmt.Sprint(dirs) != "[/ /empty /sub /sub/deep]" {
		t.Errorf("dirs = %v, want every listed directory in walk order", dirs)
	}
}
