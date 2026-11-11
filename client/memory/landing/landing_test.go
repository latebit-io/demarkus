package landing_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/client/memory/hub"
	"github.com/latebit-io/demarkus/client/memory/landing"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

func docs(backend docwrite.Backend) hub.Docs {
	return landing.Beside(&docwrite.Doc{Backend: backend, Host: "host:6309", ReadToken: "tok", Write: docwrite.SendOnce("tok")})
}

func store(docs map[string]string) *fetchtest.Client {
	published := map[string]fetch.Result{}
	for path, body := range docs {
		published["host:6309"+path] = fetchtest.Head(body, 1, map[string]string{"tags": "index"})
	}
	return &fetchtest.Client{Published: published}
}

func TestPublishLinksAFirstVersionFromItsNearestHub(t *testing.T) {
	backend := store(map[string]string{"/index.md": "# Store\n", "/proj/index.md": "# Proj\n"})
	req := &landing.PublishRequest{Path: "/proj/plan.md", Write: docwrite.Write{Body: "# Plan\n\nSteps to ship the release, in order.\n", Metadata: map[string]string{"tags": "plan"}}, Mode: merge.OnConflictFail, Stamp: map[string]string{"agent": "cli"}}
	out, err := landing.Publish(t.Context(), docs(backend), req)
	if err != nil || !out.Landed || out.Hub != (landing.HubLink{Hub: "/proj/index.md", Added: true}) || out.StyleFindings != "" {
		t.Fatalf("publish = %+v, %v", out, err)
	}
	if hubWrite := backend.PublishCall("/proj/index.md"); hubWrite == nil || !strings.Contains(hubWrite.Body, "](/proj/plan.md)") || hubWrite.Metadata["agent"] != "cli" {
		t.Errorf("hub write = %+v, want a stamped bullet for the plan", hubWrite)
	}
	// A hub that names the document already is read, not written again.
	hubWrites := func() (n int) {
		for _, call := range backend.PublishCalls {
			if call.Path == "/proj/index.md" {
				n++
			}
		}
		return n
	}
	before := hubWrites()
	if out, err := landing.Publish(t.Context(), docs(backend), req); err != nil || out.Hub.Added || out.Hub.Problem != "" || hubWrites() != before {
		t.Errorf("republish = %+v, %v; hub writes %d, want none", out.Hub, err, hubWrites()-before)
	}
}

func TestPublishSaysWhenNoHubReachesTheDocument(t *testing.T) {
	out, err := landing.Publish(t.Context(), docs(&fetchtest.Client{}), &landing.PublishRequest{Path: "/notes/quic.md", Write: docwrite.Write{Body: "# QUIC\n\nNotes.\n"}, Mode: merge.OnConflictFail})
	want := "unlinked: no hub reaches /notes/quic.md; publish /notes/index.md with a bullet for it, or add one to /index.md"
	if err != nil || out.Hub.Problem != want || out.Hub.Added {
		t.Fatalf("hub = %+v, %v", out.Hub, err)
	}
}

func TestStyleGate(t *testing.T) {
	const dashed = "# Doc\n\nWhat it is — briefly.\n"
	asked := 0
	block := func() landing.Style { asked++; return landing.Style{Strictness: publishpolicy.Block, Guide: "GUIDE"} }
	backend := &fetchtest.Client{}

	_, err := landing.Publish(t.Context(), docs(backend), &landing.PublishRequest{Path: "/doc.md", Write: docwrite.Write{Body: dashed}, Style: block})
	var refused *landing.StyleRefusal
	if !errors.As(err, &refused) || !strings.Contains(refused.Findings, "GUIDE") || len(backend.PublishCalls) != 0 {
		t.Fatalf("blocked = %v, publishes %d; want a refusal citing the guide before any write", err, len(backend.PublishCalls))
	}
	// No policy: the write lands and carries the findings under the baseline.
	out, err := landing.Publish(t.Context(), docs(backend), &landing.PublishRequest{Path: "/doc.md", Write: docwrite.Write{Body: dashed}})
	if err != nil || !out.Landed || !strings.Contains(out.StyleFindings, "em dash") {
		t.Fatalf("warned = %+v, %v", out, err)
	}
	// A clean body never asks the surface for its policy: answering may cost a read.
	asked = 0
	if _, err := landing.Publish(t.Context(), docs(backend), &landing.PublishRequest{Path: "/clean.md", Write: docwrite.Write{Body: "# Clean\n\nFine.\n"}, Style: block}); err != nil || asked != 0 {
		t.Errorf("clean publish asked for the style policy %d times, err %v", asked, err)
	}
}

func TestAConflictLandsNothingElse(t *testing.T) {
	backend := store(map[string]string{"/index.md": "# Store\n", "/doc.md": "# Doc\n\nTheirs — now.\n"})
	out, err := landing.Publish(t.Context(), docs(backend), &landing.PublishRequest{Path: "/doc.md", Write: docwrite.Write{Body: "# Doc\n\nMine — now.\n"}, Mode: merge.OnConflictFail})
	if err != nil || out.Landed || out.Result.Response.Status != protocol.StatusConflict || out.StyleFindings != "" || out.Hub != (landing.HubLink{}) {
		t.Fatalf("conflict = %+v, %v; want the server's answer and nothing more", out, err)
	}
}

func TestArchiveDropsTheBulletsThatLinkedTheDocument(t *testing.T) {
	backend := store(map[string]string{"/proj/index.md": "# Proj\n\n- [a](a.md)\n- [b](b.md)\n", "/proj/a.md": "# A\n"})
	out, err := landing.Archive(t.Context(), docs(backend), "/proj/a.md", nil)
	if err != nil || !out.Landed {
		t.Fatalf("archive = %+v, %v", out, err)
	}
	dropped := 0
	for _, h := range out.Hubs {
		if h.Hub == "/proj/index.md" {
			dropped = h.Dropped
		}
	}
	if hubWrite := backend.PublishCall("/proj/index.md"); dropped != 1 || hubWrite == nil || strings.Contains(hubWrite.Body, "a.md") {
		t.Errorf("dropped = %d, hub write = %+v", dropped, hubWrite)
	}
}

func TestArchiveUnder(t *testing.T) {
	listing := func(names ...string) func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
			if strings.TrimSuffix(r.Path, "/") == "/proj/journal" {
				return fetchtest.ListPage(r.Path, "", names...), nil
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		}
	}
	sweep := func(backend *fetchtest.Client, dir string, expected int) (landing.Swept, error) {
		return landing.ArchiveUnder(t.Context(), docs(backend), &landing.Sweep{Dir: dir, ExpectedDocs: expected, Walker: listwalk.Walker{Client: backend, Host: "host:6309", Token: "tok"}})
	}

	backend := store(map[string]string{"/proj/index.md": "# Proj\n\n- [Journal](journal/): notes\n- [keep](k.md)\n"})
	backend.ListFn = listing("a.md", "b.md")
	out, err := sweep(backend, "/proj/journal/", 2)
	if err != nil || !out.Complete || len(out.Archived) != 2 || len(backend.ArchiveCalls) != 2 {
		t.Fatalf("sweep = %+v, %v", out, err)
	}
	if hubWrite := backend.PublishCall("/proj/index.md"); hubWrite == nil || hubWrite.Body != "# Proj\n\n- [keep](k.md)\n" {
		t.Errorf("hub write = %+v, want the directory's bullet dropped", hubWrite)
	}

	refusals := []struct {
		name     string
		dir      string
		expected int
		names    []string
		want     string
	}{
		{"a document", "/proj/a.md", 0, nil, "recursive needs a directory url ending in /"},
		{"the store root", "/", 0, nil, "recursive archive of the store root is refused"},
		{"an empty directory", "/proj/journal/", 0, nil, "archive failed: no live documents under /proj/journal/"},
		{"a moved count", "/proj/journal/", 3, []string{"a.md", "b.md"}, "archive refused: 2 live documents under /proj/journal/, expected 3"},
	}
	for _, r := range refusals {
		backend := &fetchtest.Client{ListFn: listing(r.names...)}
		if _, err := sweep(backend, r.dir, r.expected); err == nil || !strings.Contains(err.Error(), r.want) || len(backend.ArchiveCalls) != 0 {
			t.Errorf("%s: err = %v, archives %d; want %q before any write", r.name, err, len(backend.ArchiveCalls), r.want)
		}
	}
}
