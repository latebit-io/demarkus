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

func TestWithoutLinks(t *testing.T) {
	body := "# Hub\n\n## Plans\n\n- [a](/proj/plans/a.md): one\n- [b](plans/b.md) and [c](/other.md)\n- [j](journal/)\n- plain text\n- [ext](https://x.example/a.md)\n"
	got, n := WithoutLinks(body, "/proj/index.md", []string{"/proj/plans/a.md", "/proj/journal/"})
	want := "# Hub\n\n## Plans\n\n- [b](plans/b.md) and [c](/other.md)\n- plain text\n- [ext](https://x.example/a.md)\n"
	if n != 2 || got != want {
		t.Errorf("n = %d\n%s", n, got)
	}
	if _, n := WithoutLinks(body, "/proj/index.md", []string{"/proj/plans/"}); n != 1 {
		t.Errorf("directory target drops %d, want the bullet under it", n)
	}
}

func TestDropLinksEditsOnlyWhenABulletGoes(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"h/index.md": fetchtest.Head("# Hub\n\n- [a](/a.md)\n- [b](/b.md)\n", 2, map[string]string{"tags": "index", "retention": "1"})}}
	n, err := DropLinks(t.Context(), docOn(backend, "/index.md"), nil, &Drop{Targets: []string{"/a.md"}, Stamp: map[string]string{"agent": "x"}})
	if err != nil || n != 1 || len(backend.PublishCalls) != 1 {
		t.Fatalf("n %d err %v publishes %d", n, err, len(backend.PublishCalls))
	}
	if w := backend.PublishCalls[0]; w.Body != "# Hub\n\n- [b](/b.md)\n" || w.ExpectedVersion != 2 || w.Metadata["agent"] != "x" || w.Metadata["retention"] != "" {
		t.Errorf("publish = %+v", w)
	}
	n, err = DropLinks(t.Context(), docOn(backend, "/index.md"), nil, &Drop{Targets: []string{"/zzz.md"}})
	if err != nil || n != 0 || len(backend.PublishCalls) != 1 {
		t.Errorf("untouched hub rewritten: n %d err %v publishes %d", n, err, len(backend.PublishCalls))
	}
	for p, r := range map[string]fetch.Result{"/missing.md": {Response: protocol.Response{Status: protocol.StatusNotFound}}, "/gone.md": fetchtest.Archived()} {
		backend.Published["h"+p] = r
		if n, err := DropLinks(t.Context(), docOn(backend, p), nil, &Drop{Targets: []string{"/a.md"}}); err != nil || n != 0 {
			t.Errorf("%s: n %d err %v", p, n, err)
		}
	}
	backend.Published["h/locked.md"] = fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}
	if _, err := DropLinks(t.Context(), docOn(backend, "/locked.md"), nil, &Drop{Targets: []string{"/a.md"}}); err == nil || err.Error() != "hub /locked.md not updated: unauthorized" {
		t.Errorf("unreadable hub = %v", err)
	}
}

func TestAncestorsAndDropFromHubs(t *testing.T) {
	if got := Ancestors("/a/b/c.md"); strings.Join(got, " ") != "/a/b/index.md /a/index.md /index.md" {
		t.Errorf("ancestors = %v", got)
	}
	if got := Ancestors("/a/index.md"); strings.Join(got, " ") != "/index.md" {
		t.Errorf("own hub excluded: %v", got)
	}
	if got := Ancestors("/a/b/"); strings.Join(got, " ") != "/a/index.md /index.md" {
		t.Errorf("directory = %v", got)
	}
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"h/a/index.md": fetchtest.Head("# A\n\n- [c](b/c.md)\n", 1, nil),
		"h/index.md":   {Response: protocol.Response{Status: protocol.StatusUnauthorized}},
	}}
	docs := func(p string) *docwrite.Doc { return docOn(backend, p) }
	if got := AncestorsOf("/a/d.md", "/a/b/c.md"); strings.Join(got, " ") != "/a/b/index.md /a/index.md /index.md" {
		t.Errorf("union = %v", got)
	}
	got := DropFromHubs(t.Context(), docs, AncestorsOf("/a/b/c.md"), &Drop{Targets: []string{"/a/b/c.md"}})
	if len(got) != 3 || got[0].Dropped != 0 || got[1].Dropped != 1 || got[2].Err == nil {
		t.Errorf("drop = %+v", got)
	}
}
