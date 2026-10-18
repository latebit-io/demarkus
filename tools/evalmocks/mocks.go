package main

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol/wiretest"
)

// The eval's story: project cwd, its hub, a journal entry the session header
// names, a roadmap, and today one day after that entry.
const (
	host      = "soul"
	hubBody   = "# Cwd\n\n- [Journal](journal/): dated session notes\n- [Roadmap](roadmap.md): what is next\n"
	entryBody = "# Cwd journal: 2026-10-11\n\nFinished the parser rewrite: the recursive descent parser replaces the table driven one, every fixture in testdata passes, and the error messages now carry a line and column. The old parser still backs the CLI. Next: wire the new parser into the CLI behind the existing flags, run the fixtures through the CLI, then delete the old parser and its tests.\n"
	roadmap   = "# Roadmap\n\nWhat is next for cwd, one bullet per item.\n\n- Next: CLI on the new parser, then remove the legacy path.\n"
	notesPath = "/cwd/notes/quic-retries.md"
	notesBody = "# QUIC retries\n\nQUIC retries back off exponentially from 200ms and cap at 5s; a stream is abandoned after six retries.\n"
	modified  = "2026-10-15T12:00:00Z"
)

var (
	today    = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	roadMeta = map[string]string{"importance": "0.6", "tags": "roadmap,cwd", "type": "Plan"}
)

// mock is one canned response: the runner's frontmatter kept verbatim, the
// body rendered by a tool call over Store (empty when nil), and the literal
// arguments the runner fills in from the agent's call instead.
type mock struct {
	Rel    string
	Front  string
	Subst  map[string]string
	Store  func() *fetchtest.Client
	Render func(ctx context.Context, t *marktools.Tools) marktools.Result
}

func (m *mock) render() ([]byte, error) {
	store := &fetchtest.Client{}
	if m.Store != nil {
		store = m.Store()
	}
	var warnings []string
	tools, err := marktools.New(store, marktools.Hooks{
		Resolve: func(_ context.Context, raw string) (marktools.Target, error) {
			return marktools.Target{Host: host, Path: raw}, nil
		},
		Writer: func(context.Context, marktools.Target, string) (marktools.WriteFunc, error) {
			return docwrite.SendOnce("eval-token"), nil
		},
		Now:   func() time.Time { return today },
		Warnf: func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		return nil, err
	}
	got := m.Render(context.Background(), tools)
	if got.IsError {
		return nil, fmt.Errorf("tool failed: %s", got.Text)
	}
	if len(warnings) > 0 {
		return nil, fmt.Errorf("fixture gaps: %s", strings.Join(warnings, "; "))
	}
	text := got.Text
	for literal, placeholder := range m.Subst {
		if !strings.Contains(text, literal) {
			return nil, fmt.Errorf("%q not in the rendering, nothing to template", literal)
		}
		text = strings.ReplaceAll(text, literal, placeholder)
	}
	return []byte(m.Front + text), nil
}

// golden is the server's named wire response with its metadata overridden,
// so a write reports the version and time the story needs. The names are
// fixed in this table, so an unknown one is a programming error.
func golden(name string, meta map[string]string) fetch.Result {
	resp, err := wiretest.Load(name)
	if err != nil {
		panic(err)
	}
	if resp.Metadata == nil {
		resp.Metadata = map[string]string{}
	}
	maps.Copy(resp.Metadata, meta)
	return fetch.Result{Response: resp}
}

// answer scripts one verb with a fixed response.
func answer[R any](r fetch.Result) func(context.Context, R) (fetch.Result, error) {
	return func(context.Context, R) (fetch.Result, error) { return r, nil }
}

// cwdStore is the story's store: hub, one journal entry, roadmap at v8.
func cwdStore() *fetchtest.Client {
	meta := maps.Clone(roadMeta)
	meta["modified"] = "2026-10-14T09:00:00Z"
	return &fetchtest.Client{
		Published: map[string]fetch.Result{
			host + "/cwd/index.md":              fetchtest.Head(hubBody, 3, nil),
			host + "/cwd/journal/2026-10-11.md": fetchtest.Head(entryBody, 1, nil),
			host + "/cwd/roadmap.md":            fetchtest.Head(roadmap, 8, meta),
			host + "/cwd/roadmap.md/v8":         fetchtest.Head(roadmap, 8, meta),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if r.Path == "/cwd/journal/" {
				return fetchtest.ListPage(r.Path, "", "2026-10-11.md"), nil
			}
			return fetchtest.ListPage(r.Path, ""), nil
		},
	}
}

// publishAs is the story's store answering every PUBLISH as the server does
// when it makes version v.
func publishAs(v int) func() *fetchtest.Client {
	return func() *fetchtest.Client {
		s := cwdStore()
		s.PublishFn = answer[fetch.WriteRequest](golden("publish-created", map[string]string{"version": fmt.Sprint(v), "modified": modified}))
		return s
	}
}

// publishNotes is the first version of the notes document both publish cases save.
func publishNotes(ctx context.Context, t *marktools.Tools) marktools.Result {
	v := 0
	return t.Publish(ctx, marktools.PublishArgs{URL: notesPath, ExpectedVersion: &v, Body: notesBody, Metadata: map[string]string{"tags": "quic,retries", "importance": "0.5", "type": "Note"}})
}

func suite(tool string) string     { return mocksLeaf + "/" + tool + ".md" }
func inCase(c, tool string) string { return c + "/" + mocksLeaf + "/" + tool + ".md" }

var mocks = []mock{
	// Suite defaults: an empty store answering the tools no case scripts.
	{
		Rel:   suite("mark_fetch"),
		Subst: map[string]string{"/docs/missing.md": "{{input.url}}"},
		Store: func() *fetchtest.Client {
			return &fetchtest.Client{FetchFn: answer[fetch.FetchRequest](golden("fetch-not-found", nil))}
		},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Fetch(ctx, marktools.FetchArgs{URL: "/docs/missing.md"})
		},
	},
	{
		Rel:   suite("mark_list"),
		Subst: map[string]string{"/cwd/empty/": "{{input.url}}"},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.List(ctx, marktools.ListArgs{URL: "/cwd/empty/"})
		},
	},
	{
		Rel:   suite("mark_lookup"),
		Subst: map[string]string{"zzznothing": "{{input.query}}", "/cwd/": "{{input.url}}"},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Lookup(ctx, marktools.LookupArgs{URL: "/cwd/", Query: "zzznothing", Render: mcpfmt.Options{Envelope: &mcpfmt.Lookup}})
		},
	},
	{
		Rel: suite("mark_archive"),
		Store: func() *fetchtest.Client {
			s := cwdStore()
			s.ArchiveFn = answer[fetch.ArchiveRequest](golden("archive", map[string]string{"version": "5"}))
			return s
		},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Archive(ctx, marktools.ArchiveArgs{URL: "/cwd/old.md"})
		},
	},
	// The journal route: today's entry under the project, created.
	{
		Rel:   suite("mark_append"),
		Store: cwdStore,
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Append(ctx, marktools.AppendArgs{URL: "/cwd/", Route: marktools.RouteToday, Tags: "remember,journal", Body: "Pruned the remember skill to the harness-specific parts; the route now refuses an archived project."})
		},
	},
	// archive-gate: the document the agent must show before asking.
	{
		Rel: inCase("archive-gate", "mark_fetch"),
		Store: func() *fetchtest.Client {
			return &fetchtest.Client{Published: map[string]fetch.Result{
				host + "/cwd/old.md": fetchtest.Head("# Old parser notes\n\nNotes on the table driven parser, kept while the rewrite was in progress.\n", 4, nil),
			}}
		},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Fetch(ctx, marktools.FetchArgs{URL: "/cwd/old.md"})
		},
	},
	// context: the primer over the story's store.
	{
		Rel:   inCase("context", "mark_fetch"),
		Store: cwdStore,
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Fetch(ctx, marktools.FetchArgs{URL: "/cwd/.context"})
		},
	},
	// publish-update: fetch the roadmap at v8, republish at v9, or append.
	{
		Rel:   inCase("publish-update", "mark_fetch"),
		Store: cwdStore,
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Fetch(ctx, marktools.FetchArgs{URL: "/cwd/roadmap.md"})
		},
	},
	{
		Rel:   inCase("publish-update", "mark_publish"),
		Front: "---\nexpect:\n  body: '/- Next: CLI on the new parser, then remove the legacy path\\.\\n+- Next: ship link on publish\\./'\n---\n",
		Store: publishAs(9),
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			v := 8
			return t.Publish(ctx, marktools.PublishArgs{URL: "/cwd/roadmap.md", ExpectedVersion: &v, Body: roadmap + "- Next: ship link on publish.\n", Metadata: roadMeta})
		},
	},
	{
		Rel: inCase("publish-update", "mark_append"),
		Store: func() *fetchtest.Client {
			s := cwdStore()
			s.AppendFn = answer[fetch.WriteRequest](golden("append", map[string]string{"version": "9", "modified": modified}))
			return s
		},
		Render: func(ctx context.Context, t *marktools.Tools) marktools.Result {
			return t.Append(ctx, marktools.AppendArgs{URL: "/cwd/roadmap.md", ExpectedVersion: 8, Body: "- Next: ship link on publish.\n"})
		},
	},
	// publish-links: a first version under a project with a hub.
	{
		Rel:    inCase("publish-links", "mark_publish"),
		Store:  publishAs(1),
		Render: publishNotes,
	},
	// publish-no-hub: the same publish where no hub reaches the project.
	{
		Rel:   inCase("publish-no-hub", "mark_publish"),
		Subst: map[string]string{notesPath: "{{input.url}}"},
		Store: func() *fetchtest.Client {
			return &fetchtest.Client{PublishFn: answer[fetch.WriteRequest](golden("publish-created", map[string]string{"modified": modified}))}
		},
		Render: publishNotes,
	},
}
