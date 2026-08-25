package listwalk

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

type stubLister struct {
	listings map[string]string // dir -> body
	statuses map[string]string // dir -> non-OK status
	pages    map[string]fetch.Result
}

func (s *stubLister) ListWithOptions(_, dir, _ string, opts fetch.ListOptions) (fetch.Result, error) {
	if page, ok := s.pages[dir+"\x00"+opts.Cursor]; ok {
		return page, nil
	}
	if status, ok := s.statuses[dir]; ok {
		return fetch.Result{Response: protocol.Response{Status: status}}, nil
	}
	body, ok := s.listings[dir]
	if !ok {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	return listPage(body, true, ""), nil
}

func listPage(body string, complete bool, next string) fetch.Result {
	meta := map[string]string{
		"entries":  strconv.Itoa(len(links.Extract(body))),
		"complete": strconv.FormatBool(complete),
	}
	if next != "" {
		meta["next-cursor"] = next
	}
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: meta, Body: body}}
}

func TestWalk(t *testing.T) {
	t.Run("collects files across subdirectories", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/":    "- [a.md](a.md)\n- [sub/](sub/)\n",
			"/sub": "- [b.md](b.md)\n",
		}}
		var got []string
		w := Walker{Client: l, Host: "h", Strict: true}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		want := []string{"/a.md", "/sub/b.md"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("collects every page before completing directory", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00":     listPage("- [a.md](a.md)\n- [b.md](b.md)\n", false, "next"),
			"/\x00next": listPage("- [c.md](c.md)\n", true, ""),
		}}
		var got []string
		w := Walker{Client: l, Host: "h", Strict: true}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
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
		err := (&Walker{Client: l, Host: "h"}).Walk("/", func(string) error { return nil })
		if !errors.Is(err, fetch.ErrListCompletenessUnknown) {
			t.Fatalf("error = %v, want ErrListCompletenessUnknown", err)
		}
	})

	t.Run("page budget counts continuations", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00": listPage("- [a.md](a.md)\n", false, "next"),
		}}
		err := (&Walker{Client: l, Host: "h", MaxLists: 1}).Walk("/", func(string) error { return nil })
		if !errors.Is(err, ErrListBudget) {
			t.Fatalf("error = %v, want ErrListBudget", err)
		}
	})

	t.Run("rejects cross-page ordering drift", func(t *testing.T) {
		l := &stubLister{pages: map[string]fetch.Result{
			"/\x00":     listPage("- [b.md](b.md)\n", false, "next"),
			"/\x00next": listPage("- [a.md](a.md)\n", true, ""),
		}}
		if err := (&Walker{Client: l, Host: "h"}).Walk("/", func(string) error { return nil }); err == nil {
			t.Fatal("ordering drift accepted")
		}
	})

	t.Run("self-referencing listing terminates at MaxDepth", func(t *testing.T) {
		// A hostile server serving a self-listing at every depth mints
		// ever-deeper distinct paths; only MaxDepth stops it.
		listings := map[string]string{"/": "- [a.md](a.md)\n- [loop/](loop/)\n"}
		dir := "/loop"
		for range 6 {
			listings[dir] = "- [b.md](b.md)\n- [loop/](loop/)\n"
			dir += "/loop"
		}
		l := &stubLister{listings: listings}
		var got []string
		w := Walker{Client: l, Host: "h", MaxDepth: 3}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		// a.md + b.md at depths 1..3.
		if len(got) != 4 {
			t.Errorf("got %d files, want 4 (%v)", len(got), got)
		}
	})

	t.Run("strict walk reports depth truncation", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/":         "- [sub/](sub/)\n",
			"/sub":      "- [deep/](deep/)\n",
			"/sub/deep": "- [x.md](x.md)\n",
		}}
		err := (&Walker{Client: l, Host: "h", Strict: true, MaxDepth: 1}).Walk("/", func(string) error { return nil })
		if !errors.Is(err, ErrDepthBudget) {
			t.Fatalf("error = %v, want ErrDepthBudget", err)
		}
	})

	t.Run("list budget returns ErrListBudget", func(t *testing.T) {
		listings := map[string]string{"/": "- [d0/](d0/)\n"}
		dir := "/d0"
		for i := range 6 {
			listings[dir] = fmt.Sprintf("- [d%d/](d%d/)\n", i+1, i+1)
			dir += fmt.Sprintf("/d%d", i+1)
		}
		l := &stubLister{listings: listings}
		w := Walker{Client: l, Host: "h", MaxLists: 3}
		if err := w.Walk("/", func(string) error { return nil }); !errors.Is(err, ErrListBudget) {
			t.Fatalf("got %v, want ErrListBudget", err)
		}
	})

	t.Run("OnSkip observes lenient skips", func(t *testing.T) {
		l := &stubLister{
			listings: map[string]string{"/": "- [a.md](a.md)\n- [bad](/etc/passwd)\n- [sub/](sub/)\n"},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		var skips []string
		w := Walker{Client: l, Host: "h", OnSkip: func(p, reason string) { skips = append(skips, p+": "+reason) }}
		if err := w.Walk("/", func(string) error { return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(skips) != 2 {
			t.Errorf("skips = %v, want unauthorized listing + invalid entry", skips)
		}
	})

	t.Run("escaped entry names are decoded", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/": "- [my doc.md](my%20doc.md)\n",
		}}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/my doc.md" {
			t.Errorf("got %v, want [/my doc.md]", got)
		}
	})

	t.Run("strict errors on non-OK listing", func(t *testing.T) {
		l := &stubLister{
			listings: map[string]string{"/": "- [sub/](sub/)\n"},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		w := Walker{Client: l, Host: "h", Strict: true}
		if err := w.Walk("/", func(string) error { return nil }); err == nil {
			t.Fatal("expected error for non-OK listing in strict mode")
		}
	})

	t.Run("lenient skips non-OK listing", func(t *testing.T) {
		l := &stubLister{
			listings: map[string]string{"/": "- [a.md](a.md)\n- [sub/](sub/)\n"},
			statuses: map[string]string{"/sub": protocol.StatusUnauthorized},
		}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/a.md" {
			t.Errorf("got %v, want [/a.md]", got)
		}
	})

	t.Run("escaping entries are skipped", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/docs": "- [ok.md](ok.md)\n- [esc](../secret.md)\n- [abs](/etc/passwd)\n- [url](mark://evil.com/x.md)\n",
		}}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk("/docs", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 1 || got[0] != "/docs/ok.md" {
			t.Errorf("got %v, want [/docs/ok.md]", got)
		}
	})

	t.Run("strict errors on escaping entry", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/docs": "- [esc](../secret.md)\n",
		}}
		w := Walker{Client: l, Host: "h", Strict: true}
		err := w.Walk("/docs", func(string) error {
			t.Error("visit must not run for an invalid entry in strict mode")
			return nil
		})
		if err == nil {
			t.Fatal("expected error for escaping entry in strict mode")
		}
	})

	t.Run("visit error aborts", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/": "- [a.md](a.md)\n- [b.md](b.md)\n",
		}}
		sentinel := errors.New("stop")
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk("/", func(string) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("got %v, want sentinel", err)
		}
	})
}

func TestResolveEntry(t *testing.T) {
	tests := []struct {
		name   string
		dir    string
		dest   string
		want   Entry
		wantOK bool
	}{
		{"file at root", "/", "a.md", Entry{Path: "/a.md"}, true},
		{"file in subdir", "/docs/", "a.md", Entry{Path: "/docs/a.md"}, true},
		{"dir entry", "/", "sub/", Entry{Path: "/sub", IsDir: true}, true},
		{"escaped name decoded", "/", "my%20doc.md", Entry{Path: "/my doc.md"}, true},
		{"absolute rejected", "/docs/", "/etc/passwd", Entry{}, false},
		{"parent escape rejected", "/docs/", "../secret.md", Entry{}, false},
		{"nested escape rejected", "/docs/", "a/../../secret.md", Entry{}, false},
		{"encoded escape rejected", "/docs/", "..%2Fsecret.md", Entry{}, false},
		{"url rejected", "/", "mark://evil.com/x.md", Entry{}, false},
		{"empty rejected", "/", "", Entry{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveEntry(tt.dir, tt.dest)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("ResolveEntry(%q, %q) = (%+v, %v), want (%+v, %v)", tt.dir, tt.dest, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
