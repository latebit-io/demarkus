package marktools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/marktools"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/render"
)

const promoteSource = "# Retry Rules\n\nWhen to retry a call.\n\n## Budget\n\nThree tries.\n"

// packetOf is the packet hash a promote report carries.
func packetOf(t *testing.T, report marktools.Result) string {
	t.Helper()
	return hashAfter(t, report, "packet: ")
}

// The mechanics are tested in client/memory/promote; here that each phase reads
// the head, authorizes its write, and words the outcome.
func TestPromoteStageThenStamp(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"host:6309/adr/0001-retry.md": fetchtest.Head(promoteSource, 3, map[string]string{"tags": "retry", "type": "Decision", "importance": "0.9"}),
	}}
	tools := newTools(t, backend, writingHooks())
	stage := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/adr/0001-retry.md", Phase: "stage"})
	for _, want := range []string{"status: staged\npath: /adr/0001-retry.md\nversion: 3\n", "candidate: decision record, type Decision, importance 0.9\n", "\n\n# Retry Rules\n"} {
		if !strings.Contains(stage.Text, want) {
			t.Errorf("stage missing %q:\n%s", want, stage.Text)
		}
	}
	packet := packetOf(t, stage)
	if len(backend.PublishCalls) != 0 {
		t.Fatal("stage wrote")
	}
	for name, args := range map[string]marktools.PromoteArgs{
		"no target": {URL: "/adr/0001-retry.md", Phase: "stamp", Via: "k", Packet: packet},
		"no via":    {URL: "/adr/0001-retry.md", Phase: "stamp", Target: "mark://team/r.md@v1", Packet: packet},
		"no packet": {URL: "/adr/0001-retry.md", Phase: "stamp", Target: "mark://team/r.md@v1", Via: "k"},
		"locked":    {URL: "/locked.md", Phase: "stamp", Target: "mark://team/r.md@v1", Via: "k", Packet: packet},
		"phase":     {URL: "/adr/0001-retry.md", Phase: "later"},
	} {
		if got := tools.Promote(t.Context(), args); !got.IsError || len(backend.PublishCalls) != 0 {
			t.Errorf("%s = %+v", name, got)
		}
	}
	got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/adr/0001-retry.md", Phase: "stamp", Target: "mark://team/r.md@v1", Via: "knowledge", Packet: packet})
	if got.IsError || got.Text != "status: stamped\npath: /adr/0001-retry.md\nversion: 1\nmode: full\n" {
		t.Fatalf("stamp = %+v", got)
	}
	w := backend.PublishCalls[0]
	if w.ExpectedVersion != 3 || w.Token != "write-token" || w.Metadata["tags"] != "retry,promoted" || w.Metadata["agent"] != "test-agent" || !strings.HasPrefix(w.Body, "# Retry Rules\n\npromoted-via: knowledge\npromoted: mark://team/r.md@v1\n\nWhen to retry") {
		t.Errorf("publish = %+v", w)
	}
	again := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/adr/0001-retry.md", Phase: "stage"})
	if !strings.Contains(again.Text, "status: promoted\n") || !strings.Contains(again.Text, "promoted-via: knowledge\npromoted: mark://team/r.md@v1\nmode: full\n") {
		t.Errorf("restaged:\n%s", again.Text)
	}
}

func TestPromoteRefreshReportsThenWritesWithThePacket(t *testing.T) {
	stamped := "# Retry Rules\n\npromoted-via: knowledge\npromoted: mark://team/r.md@v1\n\nWhen to retry a call.\n"
	backend := &fetchtest.Client{Published: map[string]fetch.Result{"host:6309/r.md": fetchtest.Head(stamped, 2, map[string]string{"tags": "retry,promoted"})}}
	tools := newTools(t, backend, writingHooks())
	live := "# Retry rules, revised\n\nWhen to retry, and when not.\n"
	if got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/r.md", Phase: "refresh", Body: live, Target: "mark://team/r.md@v1"}); got.IsError || !strings.HasPrefix(got.Text, "status: in-sync\n") {
		t.Fatalf("in sync = %+v", got)
	}
	report := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/r.md", Phase: "refresh", Body: live, Target: "mark://team/r.md@v3"})
	if !strings.HasPrefix(report.Text, "status: stale\npath: /r.md\nversion: 2\ntracked: v1\nlive: v3\nmode: stub\n") {
		t.Fatalf("report = %+v", report)
	}
	packet := packetOf(t, report)
	if got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/r.md", Phase: "refresh", Body: live, Target: "mark://team/r.md@v3", Packet: "stale"}); !got.IsError || !strings.Contains(got.Text, "does not match") {
		t.Errorf("stale packet = %+v", got)
	}
	got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/r.md", Phase: "refresh", Body: live, Target: "mark://team/r.md@v3", Packet: packet})
	if got.IsError || got.Text != "status: refreshed\npath: /r.md\nversion: 1\nmode: stub\n" {
		t.Fatalf("apply = %+v", got)
	}
	if w := backend.PublishCalls[0]; w.ExpectedVersion != 2 || w.Body != "# Retry Rules\n\npromoted-via: knowledge\npromoted: mark://team/r.md@v3\n\nWhen to retry, and when not.\n" {
		t.Errorf("publish = %+v", w)
	}
}

// Refresh on a directory with no body is the inventory the refresh command
// used to assemble from a capped tag lookup: every promoted document, walked.
func TestPromoteRefreshOnADirectoryInventoriesPromotedDocuments(t *testing.T) {
	backend := &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309/cwd/index.md":   fetchtest.Head("# Cwd\n\n- [a](a.md)\n", 1, nil),
			"host:6309/cwd/a.md":       fetchtest.Head("# A\n\npromoted-via: team\npromoted: mark://latebit/a.md@v3\n\nBody.\n", 2, nil),
			"host:6309/cwd/b.md":       fetchtest.Head("# B\n\nLocal only.\n", 1, nil),
			"host:6309/cwd/old/leg.md": fetchtest.Head("# Legacy\n\npromoted: mark://old/leg.md\n", 1, nil),
			"host:6309/cwd/old/tag.md": fetchtest.Head("# Tagged\n", 1, map[string]string{"tags": "promoted"}),
		},
		ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			switch strings.TrimSuffix(r.Path, "/") {
			case "/cwd":
				return fetchtest.ListPage(r.Path, "", "a.md", "b.md", "index.md", "old/"), nil
			case "/cwd/old":
				return fetchtest.ListPage(r.Path, "", "leg.md", "tag.md"), nil
			}
			return fetchtest.ListPage(r.Path, ""), nil
		},
	}
	tools := newTools(t, backend, directHooks())
	got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/cwd/", Phase: "refresh"})
	want := "status: inventory\npath: /cwd/\ndocuments: 5\npromoted: 3\n" +
		"- /cwd/a.md: promoted-via team; promoted mark://latebit/a.md@v3\n" +
		"- /cwd/old/leg.md: promoted-via none (legacy marker); promoted mark://old/leg.md\n" +
		"- /cwd/old/tag.md: tag only, no marker\n"
	if got.IsError || got.Text != want {
		t.Fatalf("inventory = %+v\nwant\n%s", got, want)
	}
	// One document with no body answers its own row from its head: no walk.
	backend.ListCalls = nil
	got = tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/cwd/a.md", Phase: "refresh"})
	want = "status: inventory\npath: /cwd/a.md\ndocuments: 1\npromoted: 1\n- /cwd/a.md: promoted-via team; promoted mark://latebit/a.md@v3\n"
	if got.IsError || got.Text != want || len(backend.ListCalls) != 0 {
		t.Errorf("one document = %+v (lists %d)\nwant\n%s", got, len(backend.ListCalls), want)
	}
	if got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/cwd/b.md", Phase: "refresh"}); got.IsError || got.Text != "status: inventory\npath: /cwd/b.md\ndocuments: 1\npromoted: 0\n" {
		t.Errorf("an unpromoted document = %+v", got)
	}
	if got := tools.Promote(t.Context(), marktools.PromoteArgs{URL: "/cwd/a.md", Phase: "refresh", Target: "mark://latebit/a.md@v3"}); !got.IsError || !strings.Contains(got.Text, "refresh needs body") {
		t.Errorf("a target without a body = %+v", got)
	}
}

// knowledgeBackend is a world with a blocking policy and one document the
// draft duplicates by title.
func knowledgeBackend() *fetchtest.Client {
	return &fetchtest.Client{
		Published: map[string]fetch.Result{
			"host:6309" + publishpolicy.DocumentPath: fetchtest.Head("# Policy\n\nstrictness: block\nrequire_tags: domain\n", 1, nil),
			"host:6309/docs/retries.md":              fetchtest.Head("# Retry Rules\n\nOlder rules.\n", 2, map[string]string{"tags": "retry"}),
		},
		LookupFn: func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
			switch r.Match {
			case fetch.MatchBody:
				return fetchtest.Lookup(r.Query, r.Scope, r.Match,
					render.LookupRow{Path: "/docs/retries.md", Title: "Retry Rules", Tags: []string{"retry"}, Importance: 0.5},
					render.LookupRow{Path: "/docs/other.md", Title: "Other", Tags: []string{"misc"}, Importance: 0.3},
				), nil
			case fetch.MatchSimilar:
				// The store's near-duplicate judgment: the same-title document
				// reworded, and a section of a hub the lookups never surfaced.
				// The target itself answers too when it exists: an update is not its own duplicate.
				return fetchtest.Lookup(r.Query, r.Scope, r.Match,
					render.LookupRow{Path: "/docs/retry-rules.md", Title: "Retry Rules", Tags: []string{"retry"}, Importance: 0.5, Snippet: "near-duplicate (0.98 shared)"},
					render.LookupRow{Path: "/docs/retries.md", Title: "Retry Rules", Tags: []string{"retry"}, Importance: 0.5, Snippet: "near-duplicate (0.76 shared)"},
					render.LookupRow{Path: "/docs/hub.md", Anchor: "retries", Title: "Hub › Retries", Tags: []string{"hub"}, Importance: 0.9, Snippet: "contained (0.91)"},
				), nil
			}
			return fetchtest.Lookup(r.Query, r.Scope, ""), nil
		},
	}
}

func TestPromoteDryRunChecksPolicyAndDedups(t *testing.T) {
	backend := knowledgeBackend()
	tools := newTools(t, backend, writingHooks())
	draft := marktools.PromoteArgs{URL: "/docs/retry-rules.md", Phase: "dry_run", Body: promoteSource, Metadata: map[string]string{"tags": "retry", "importance": "0.7", "retention": "1000000", "pinned": "true"}}
	got := tools.Promote(t.Context(), draft)
	// Dedup finds the same-title row; the packet is over the values the write carries.
	draft.Phase, draft.Packet = "apply", packetOf(t, got)
	if applied := tools.Promote(t.Context(), draft); applied.IsError || backend.PublishCalls[0].Metadata["retention"] != "1000000" || backend.PublishCalls[0].Metadata["pinned"] != "true" || backend.PublishCalls[0].Metadata["importance"] != "0.7" {
		t.Errorf("numbers = %+v (%s)", backend.PublishCalls, applied.Text)
	}
	backend.PublishCalls, draft.Phase, draft.Packet = nil, "dry_run", ""
	for _, want := range []string{"target: /docs/retry-rules.md\nstatus: findings\nversion: 0\npolicy: strictness block; require_tags domain\n", "findings:\n- missing tag axis domain", "candidates:\n- /docs/retries.md: same title, near-duplicate (0.76 shared) (v2); tags retry\n- /docs/hub.md: contained (0.91) in #retries; tags hub\n", "related:\n- /docs/other.md: Other\n", "packet: ", "\n=== /docs/retries.md (v2)\n# Retry Rules\n\nOlder rules."} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("dry run missing %q:\n%s", want, got.Text)
		}
	}
	if got.IsError || len(backend.PublishCalls) != 0 || strings.Contains(got.Text, "/docs/retry-rules.md: near-duplicate") {
		t.Fatalf("dry run = %+v", got)
	}
	draft.URL = "/locked.md"
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.HasPrefix(got.Text, "status: not-permitted\npublish requires a token") {
		t.Errorf("unwritable = %+v", got)
	}
	draft.URL, draft.Body = "/docs/retry-rules.md", ""
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "needs body") {
		t.Errorf("no body = %+v", got)
	}
	// An older server refuses the similar mode: the dry run says so and stands.
	backend.LookupFn = func(_ context.Context, r fetch.LookupRequest) (fetch.Result, error) {
		if r.Match == fetch.MatchSimilar {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest}}, nil
		}
		return fetchtest.Lookup(r.Query, r.Scope, r.Match), nil
	}
	draft.Body = promoteSource
	if got := tools.Promote(t.Context(), draft); got.IsError || !strings.Contains(got.Text, "note: near-duplicate lookup not offered by this server; dedup by title and tags only\n") {
		t.Errorf("older server = %+v", got)
	}
	backend.LookupFn = func(context.Context, fetch.LookupRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}, nil
	}
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.HasPrefix(got.Text, "status: incomplete-dedup\n") {
		t.Errorf("lookup failure = %+v", got)
	}
}

func TestPromoteApplyBindsToThePacketAndTheStyleGate(t *testing.T) {
	backend := knowledgeBackend()
	hooks := writingHooks()
	hooks.Style = func(context.Context, marktools.Target) marktools.StyleGate {
		return marktools.StyleGate{Strictness: publishpolicy.Block}
	}
	tools := newTools(t, backend, hooks)
	draft := marktools.PromoteArgs{URL: "/docs/retry-rules.md", Phase: "dry_run", Body: promoteSource, Metadata: map[string]string{"tags": "retry,domain:eng"}, Source: "soul"}
	packet := packetOf(t, tools.Promote(t.Context(), draft))
	draft.Phase = "apply"
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "apply needs the packet") {
		t.Errorf("no packet = %+v", got)
	}
	// A changed draft or metadata is a new draft: the old packet no longer binds it.
	draft.Packet, draft.Body = packet, "# Retry Rules\n\nWhen to retry a call.\n"
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "does not match the draft") {
		t.Errorf("changed body = %+v", got)
	}
	draft.Body, draft.Metadata = promoteSource, map[string]string{"tags": "retry"}
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "does not match the draft") {
		t.Errorf("changed metadata = %+v", got)
	}
	// The style gate holds apply as it holds mark_publish: block refuses.
	draft.Body, draft.Metadata, draft.Phase = "# Retry Rules\n\nWhen — to retry.\n", map[string]string{"tags": "retry,domain:eng"}, "dry_run"
	draft.Packet = packetOf(t, tools.Promote(t.Context(), draft))
	draft.Phase = "apply"
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "publish refused, style problems") || len(backend.PublishCalls) != 0 {
		t.Errorf("blocked style = %+v", got)
	}
	draft.Body, draft.Phase, draft.Packet = promoteSource, "apply", packet
	got := tools.Promote(t.Context(), draft)
	// The landing is mark_publish's: an unlinked document is noted the same way.
	if got.IsError || !strings.HasPrefix(got.Text, "status: published\ntarget: /docs/retry-rules.md\nversion: 1\n") || !strings.Contains(got.Text, "\nnote: unlinked: no hub reaches /docs/retry-rules.md;") {
		t.Fatalf("apply = %+v", got)
	}
	w := backend.PublishCalls[0]
	if w.ExpectedVersion != 0 || w.Token != "write-token" || w.Metadata["tags"] != "retry,domain:eng" || w.Metadata["promoted-from"] != "soul" || w.Metadata["agent"] != "test-agent" || w.Metadata["user"] != "alice@example.com" {
		t.Errorf("publish = %+v", w)
	}
	// A conflict at the target is refused, never merged.
	draft.Phase = "dry_run"
	draft.Packet = packetOf(t, tools.Promote(t.Context(), draft))
	backend.Published["host:6309/docs/retry-rules.md"] = fetchtest.Head(promoteSource, 2, nil)
	draft.Phase = "apply"
	if got := tools.Promote(t.Context(), draft); !got.IsError || !strings.Contains(got.Text, "does not match the draft against the target at v2") {
		t.Errorf("moved target = %+v", got)
	}
}

// A dry run judges the draft under the floor world's policy merged with the
// target's, as the server will, so apply cannot be refused by a floor the
// dry run never saw.
func TestPromoteDryRunMergesTheFloorPolicy(t *testing.T) {
	backend := &fetchtest.Client{Published: map[string]fetch.Result{
		"team" + publishpolicy.DocumentPath: fetchtest.Head("# Policy\n\nstrictness: warn\n", 1, nil),
		"root" + publishpolicy.DocumentPath: fetchtest.Head("# Policy\n\nstrictness: block\nrequire_tags: category\nallow_tags.category: guide\n", 1, nil),
	}}
	hooks := writingHooks()
	hooks.Resolve = func(_ context.Context, raw string) (marktools.Target, error) {
		return marktools.Target{Host: "team", Path: strings.TrimPrefix(raw, "mark://team")}, nil
	}
	hooks.Floor = func(_ context.Context, target marktools.Target) (marktools.Target, bool) {
		return marktools.Target{Host: "root", Path: "/"}, target.Host != "root"
	}
	tools := newTools(t, backend, hooks)
	got := tools.Promote(t.Context(), marktools.PromoteArgs{
		URL: "mark://team/notes/a.md", Phase: "dry_run",
		Body:     "# A\n\nWhat A settles for its readers.\n",
		Metadata: map[string]string{"tags": "category:howto", "importance": "0.5", "type": "Guide"},
	})
	if got.IsError || !strings.Contains(got.Text, "findings:") || !strings.Contains(got.Text, "tag category:howto (allowed category values: guide)") {
		t.Fatalf("dry run under the floor = %+v", got)
	}
}
