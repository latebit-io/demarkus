package hub

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

func docOn(backend *fetchtest.Client, p string) *docwrite.Doc {
	return &docwrite.Doc{Backend: backend, Host: "h", Path: p, Write: docwrite.SendOnce("w")}
}

func TestWithBullet(t *testing.T) {
	got := WithBullet("# H\n\n## Journal\n\n- a\n\n## Next\n\nx\n", "## Journal", "- b")
	if got != "# H\n\n## Journal\n\n- a\n- b\n\n## Next\n\nx\n" {
		t.Errorf("under section = %q", got)
	}
	if got := WithBullet("# H\n", "## Journal", "- b"); got != "# H\n\n## Journal\n\n- b\n" {
		t.Errorf("new section = %q", got)
	}
	if got := WithBullet("# H\n\n- a\n", "", "- b"); got != "# H\n\n- a\n- b\n" {
		t.Errorf("no section = %q", got)
	}
}

func TestReaches(t *testing.T) {
	hub := "/proj/index.md"
	for body, want := range map[string]bool{
		"- [J](journal/)":                     true,
		"- [d](journal/2026-10-11.md)":        true,
		"- [d](/proj/journal/x.md)":           true,
		"- [p](plans/p.md)":                   false,
		"- [ext](https://x.example/journal/)": false,
	} {
		if got := Reaches(body, hub, "/proj/journal/"); got != want {
			t.Errorf("Reaches(%q) = %v, want %v", body, got, want)
		}
	}
	if !Reaches("- [p](/proj/)", "/index.md", "/proj/") || !Reaches("- [p](/proj)", "/index.md", "/proj/") {
		t.Error("a project link reaches the project directory")
	}
}

func TestTitle(t *testing.T) {
	if got := Title("# Demarkus\n\nx\n", map[string]string{"title": "Meta"}, "slug"); got != "Demarkus" {
		t.Errorf("H1 wins: %q", got)
	}
	if got := Title("no heading\n", map[string]string{"title": "Meta"}, "slug"); got != "Meta" {
		t.Errorf("metadata title next: %q", got)
	}
	if got := Title("", nil, "slug"); got != "slug" {
		t.Errorf("fallback last: %q", got)
	}
}

func TestEnsureLinkCreatesAddsOrLeavesAlone(t *testing.T) {
	link := &Link{Target: "/proj/journal/", Bullet: "- [Journal](journal/)", Section: "## Journal",
		CreateBody: "# proj\n\n## Journal\n\n- [Journal](journal/)\n", CreateMeta: map[string]string{"tags": "index"}, Stamp: map[string]string{"agent": "a"}}

	backend := &fetchtest.Client{}
	if err := EnsureLink(t.Context(), docOn(backend, "/proj/index.md"), nil, link); err != nil {
		t.Fatal(err)
	}
	if len(backend.PublishCalls) != 1 || backend.PublishCalls[0].ExpectedVersion != 0 || backend.PublishCalls[0].Metadata["agent"] != "a" {
		t.Fatalf("create = %+v", backend.PublishCalls)
	}

	backend = &fetchtest.Client{Published: map[string]fetch.Result{"h/proj/index.md": fetchtest.Head("# proj\n\n## Plans\n\n- [p](plans/p.md)\n", 3, map[string]string{"tags": "index", "retention": "2", "agent": "old"})}}
	if err := EnsureLink(t.Context(), docOn(backend, "/proj/index.md"), nil, link); err != nil {
		t.Fatal(err)
	}
	w := backend.PublishCalls[0]
	if w.ExpectedVersion != 3 || !strings.HasSuffix(w.Body, "## Plans\n\n- [p](plans/p.md)\n\n## Journal\n\n- [Journal](journal/)\n") || w.Metadata["agent"] != "a" || w.Metadata["retention"] != "" {
		t.Errorf("add = %+v", w)
	}

	backend = &fetchtest.Client{Published: map[string]fetch.Result{"h/proj/index.md": fetchtest.Head("# proj\n\n- [J](journal/)\n", 3, nil)}}
	if err := EnsureLink(t.Context(), docOn(backend, "/proj/index.md"), nil, link); err != nil || len(backend.PublishCalls) != 0 {
		t.Errorf("linked hub rewritten: %v %+v", err, backend.PublishCalls)
	}

	backend = &fetchtest.Client{Published: map[string]fetch.Result{"h/proj/index.md": {Response: protocol.Response{Status: protocol.StatusUnauthorized}}}}
	if err := EnsureLink(t.Context(), docOn(backend, "/proj/index.md"), nil, link); err == nil || err.Error() != "hub /proj/index.md not updated: unauthorized" {
		t.Errorf("unreadable hub = %v", err)
	}
}

func TestEnsureLinkRetriesOnceOnConflict(t *testing.T) {
	calls := 0
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"h/index.md": fetchtest.Head("# Projects\n", 1, nil)},
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			calls++
			if calls == 1 {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "2"}}}, nil
		},
	}
	link := &Link{Target: "/proj/", Bullet: "- [proj](/proj/)", CreateBody: "x"}
	if err := EnsureLink(t.Context(), docOn(backend, "/index.md"), nil, link); err != nil || calls != 2 {
		t.Fatalf("err %v calls %d", err, calls)
	}
}
