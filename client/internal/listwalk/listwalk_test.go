package listwalk

import (
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

type stubLister struct {
	listings map[string]string // dir -> body
	statuses map[string]string // dir -> non-OK status
}

func (s *stubLister) List(_, dir, _ string) (fetch.Result, error) {
	if status, ok := s.statuses[dir]; ok {
		return fetch.Result{Response: protocol.Response{Status: status}}, nil
	}
	body, ok := s.listings[dir]
	if !ok {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body}}, nil
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

	t.Run("listing cycle terminates", func(t *testing.T) {
		l := &stubLister{listings: map[string]string{
			"/":     "- [loop/](loop/)\n- [a.md](a.md)\n",
			"/loop": "- [loop/](loop/)\n- [b.md](b.md)\n",
		}}
		var got []string
		w := Walker{Client: l, Host: "h"}
		if err := w.Walk("/", func(p string) error { got = append(got, p); return nil }); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d files, want 2 (%v)", len(got), got)
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
			listings: map[string]string{"/": "- [sub/](sub/)\n- [a.md](a.md)\n"},
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
