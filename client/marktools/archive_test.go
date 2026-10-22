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

func TestArchiveDropsTheBulletsThatLinkedTheDocument(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/proj/plans/index.md": fetchtest.Head("# Plans\n\n- [a](a.md)\n- [b](b.md)\n", 1, map[string]string{"tags": "index"}),
		"host:6309/proj/index.md":       fetchtest.Head("# Proj\n\n- [plans](plans/)\n- [a](plans/a.md) and [b](plans/b.md)\n", 2, nil),
		"host:6309/index.md":            fetchtest.Head("# Root\n\n- [proj](/proj/)\n", 1, nil),
	}}
	tools := newTools(t, backend, writingHooks())
	got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: "/proj/plans/a.md"})
	if got.IsError || got.Text != "status: ok\nversion: 3\narchived: true\nhubs:\n- /proj/plans/index.md: 1 bullet(s) dropped\n" {
		t.Fatalf("archive = %+v", got)
	}
	if len(backend.PublishCalls) != 1 || backend.PublishCalls[0].Body != "# Plans\n\n- [b](b.md)\n" || backend.PublishCalls[0].Metadata["agent"] != "test-agent" {
		t.Errorf("hub publishes = %+v", backend.PublishCalls)
	}
}

func TestArchiveRecursiveSweepsADirectoryThenUnlinksIt(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309/proj/index.md": fetchtest.Head("# Proj\n\n## Journal\n\n- [Journal](journal/): dated notes\n- [keep](plans/k.md)\n", 2, nil),
			"host:6309/index.md":      fetchtest.Head("# Root\n\n- [proj](/proj/)\n", 1, nil),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			switch strings.TrimSuffix(r.Path, "/") {
			case "/proj/journal":
				return fetchtest.ListPage(r.Path, "", "2026-10-11.md", "2026-10-12.md", "old/"), nil
			case "/proj/journal/old":
				return fetchtest.ListPage(r.Path, "", "2025-01-01.md"), nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		},
	}
	tools := newTools(t, backend, writingHooks())
	got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: "/proj/journal/", Recursive: true})
	want := "status: archived\narchived: 3 of 3\n- /proj/journal/2026-10-11.md: v3\n- /proj/journal/2026-10-12.md: v3\n- /proj/journal/old/2025-01-01.md: v3\nhubs:\n- /proj/index.md: 1 bullet(s) dropped\n"
	if got.IsError || got.Text != want {
		t.Fatalf("archive = %+v", got)
	}
	if len(backend.ArchiveCalls) != 3 || backend.ArchiveCalls[0].Token != "write-token" || backend.ListCalls[0].IncludeArchived {
		t.Errorf("archives = %+v lists = %+v", backend.ArchiveCalls, backend.ListCalls)
	}
	if len(backend.PublishCalls) != 1 || backend.PublishCalls[0].Body != "# Proj\n\n## Journal\n\n- [keep](plans/k.md)\n" {
		t.Errorf("hub publishes = %+v", backend.PublishCalls)
	}
}

func TestArchiveRecursiveKeepsDirectoryLinksAfterAFailure(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{"host:6309/index.md": fetchtest.Head("# Root\n\n- [d](/d/)\n- [a](/d/a.md)\n", 1, nil)},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			return fetchtest.ListPage(r.Path, "", "a.md", "b.md"), nil
		},
		ArchiveFn: func(_ context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
			if r.Path == "/d/b.md" {
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "2", "archived": "true"}}}, nil
		},
	}
	tools := newTools(t, backend, writingHooks())
	got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: "/d/", Recursive: true})
	if got.IsError || !strings.HasPrefix(got.Text, "status: partial\narchived: 1 of 2\n- /d/a.md: v2\n- /d/b.md: failed: unauthorized\nhubs:\n- /index.md: 1 bullet(s) dropped\n") {
		t.Fatalf("archive = %+v", got)
	}
	if backend.PublishCalls[0].Body != "# Root\n\n- [d](/d/)\n" {
		t.Errorf("hub = %q", backend.PublishCalls[0].Body)
	}
}

func TestArchiveRecursiveRefusals(t *testing.T) {
	backend := &fetchtest.Client{ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		return fetchtest.ListPage(r.Path, ""), nil
	}}
	tools := newTools(t, backend, writingHooks())
	for url, want := range map[string]string{"/a.md": "recursive needs a directory url ending in /", "/": "store root is refused", "/empty/": "no live documents under /empty/"} {
		if got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: url, Recursive: true}); !got.IsError || !strings.Contains(got.Text, want) {
			t.Errorf("%s = %+v", url, got)
		}
	}
	if got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: "/locked.md"}); !got.IsError || got.Text != "archive requires a token" {
		t.Errorf("unauthorized = %+v", got)
	}
	if len(backend.ArchiveCalls) != 0 {
		t.Error("a refusal archived")
	}
}

func TestArchiveRecursiveRefusesAMovedCount(t *testing.T) {
	backend := &fetchtest.Client{ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		return fetchtest.ListPage(r.Path, "", "a.md", "b.md"), nil
	}}
	tools := newTools(t, backend, writingHooks())
	got := tools.Archive(t.Context(), marktools.ArchiveArgs{URL: "/d/", Recursive: true, ExpectedDocs: 1})
	if !got.IsError || got.Text != "archive refused: 2 live documents under /d/, expected 1; the directory changed since it was listed, list and gate again" {
		t.Fatalf("archive = %+v", got)
	}
	if len(backend.ArchiveCalls) != 0 {
		t.Error("a refused sweep archived")
	}
}
