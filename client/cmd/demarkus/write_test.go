package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

func cliDoc(backend docwrite.Backend) *docwrite.Doc {
	return &docwrite.Doc{
		Backend: backend, Host: "host:6309", Path: "/doc.md", ReadToken: "tok",
		Write: docwrite.SendOnce("tok"),
	}
}

func conflicted(current, base string) *fetchtest.Client {
	return &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict, Metadata: map[string]string{"server-version": "6"}}}, nil
		},
		FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			body, version := current, "6"
			if strings.HasSuffix(r.Path, "/v5") {
				body, version = base, "5"
			}
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{"version": version}}}, nil
		},
	}
}

// Decision 4: a publish says which version it replaces, or says -force.
func TestPublishNeedsAVersionOrForce(t *testing.T) {
	backend := &fetchtest.Client{}
	_, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(backend), body: "# Doc"})
	if err == nil || !strings.Contains(err.Error(), "-expected-version") || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("err = %v, want it to name both ways out", err)
	}
	_, err = runWrite(t.Context(), &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(backend), body: "# Doc", expectedVersion: new(-1)})
	if err == nil || len(backend.PublishCalls) != 0 {
		t.Fatalf("err = %v, publishes = %d: a negative version is not a way to skip the check any more", err, len(backend.PublishCalls))
	}

	forced, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(backend), body: "# Doc", force: true})
	if err != nil || forced.code != 0 || len(backend.PublishCalls) != 1 || backend.PublishCalls[0].ExpectedVersion != -1 {
		t.Fatalf("forced = %+v, %v, calls = %+v", forced, err, backend.PublishCalls)
	}
	created, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(backend), body: "# Doc", expectedVersion: new(0)})
	if err != nil || created.code != 0 || backend.PublishCalls[1].ExpectedVersion != 0 {
		t.Fatalf("create only = %+v, %v", created, err)
	}
}

// C8: a conflict hands back a merged candidate, as the MCP tool does.
func TestPublishConflictPrintsAMergeCandidate(t *testing.T) {
	w := &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(conflicted("a\nb\nC\n", "a\nb\nc\n")), body: "a\nB\nc\n", expectedVersion: new(5)}
	got, err := runWrite(t.Context(), w)
	if err != nil {
		t.Fatal(err)
	}
	if got.code != 1 || got.stdout != "a\nB\nC\n" {
		t.Errorf("outcome = %+v, want the candidate on stdout and a failing exit", got)
	}
	for _, want := range []string{"[merge-candidate]", "your-version=5", "current-version=6", "publish-at-version=6", "has-markers=false"} {
		if !strings.Contains(got.notice, want) {
			t.Errorf("notice %q missing %q", got.notice, want)
		}
	}

	w.onConflict = "fail"
	failed, err := runWrite(t.Context(), w)
	if err != nil || failed.code != 1 || failed.status != protocol.StatusConflict || failed.stdout != "" {
		t.Errorf("fail mode = %+v, %v, want the bare conflict", failed, err)
	}
}

// C9: append learns the version itself, as the MCP tool does.
func TestAppendResolvesItsVersion(t *testing.T) {
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 7), nil
		},
	}
	got, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbAppend, doc: cliDoc(backend), body: "more"})
	if err != nil || got.code != 0 || backend.AppendCalls[0].ExpectedVersion != 7 {
		t.Fatalf("append = %+v, %v, calls = %+v", got, err, backend.AppendCalls)
	}
	explicit, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbAppend, doc: cliDoc(backend), body: "more", expectedVersion: new(3)})
	if err != nil || explicit.code != 0 || backend.AppendCalls[1].ExpectedVersion != 3 || len(backend.VersionsCalls) != 1 {
		t.Fatalf("explicit = %+v, %v", explicit, err)
	}
}

func TestAnUnknownOutcomeSaysDoNotResend(t *testing.T) {
	lost := fmt.Errorf("read response: %w", fetch.ErrOutcomeUnknown)
	backend := &fetchtest.Client{
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) { return fetch.Result{}, lost },
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"version": "3"}}}, nil
		},
	}
	_, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbArchive, doc: cliDoc(backend)})
	if !errors.Is(err, fetch.ErrOutcomeUnknown) || !strings.Contains(err.Error(), "may have landed") {
		t.Fatalf("err = %v, want the unknown outcome with the advice", err)
	}
}

// edit never loses what the user typed: a conflict saves the merged candidate,
// any other failure saves the edits, and both say where.
func TestFinishEditSavesWorkOnEveryFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	saved := func(t *testing.T, notice string) string {
		t.Helper()
		_, rest, ok := strings.Cut(notice, "saved to ")
		if !ok {
			t.Fatalf("notice %q names no file", notice)
		}
		data, err := os.ReadFile(strings.Fields(rest)[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	conflict := finishEdit(t.Context(), cliDoc(conflicted("a\nb\nC\n", "a\nb\nc\n")), editedDoc{body: "a\nB\nc\n", fetchedVersion: 5})
	if conflict.code != 1 || saved(t, conflict.notice) != "a\nB\nC\n" || !strings.Contains(conflict.notice, "-expected-version 6") {
		t.Errorf("conflict = %+v", conflict)
	}

	refused := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{}, errors.New("dial refused")
	}}
	failed := finishEdit(t.Context(), cliDoc(refused), editedDoc{body: "my edits", fetchedVersion: 5})
	if failed.code != 1 || saved(t, failed.notice) != "my edits" || !strings.Contains(failed.notice, "dial refused") {
		t.Errorf("failed publish = %+v", failed)
	}

	ok := finishEdit(t.Context(), cliDoc(&fetchtest.Client{}), editedDoc{body: "my edits", fetchedVersion: 0})
	if ok.code != 0 || ok.status != protocol.StatusOK {
		t.Errorf("published = %+v", ok)
	}
}

// PUBLISH replaces the metadata map, so an edit that sends none strips the
// document's tags. What was fetched goes back, minus the server's own keys.
func TestEditCarriesTheDocumentsMetadata(t *testing.T) {
	fetched := protocol.Response{Status: protocol.StatusOK, Body: "old", Metadata: map[string]string{
		"version": "5", "modified": "2026-01-01T00:00:00Z", "etag": "e", "content-hash": "sha256-x",
		"tags": "adr,decision", "importance": "0.8", "type": "Decision",
	}}
	edited, err := editedFrom(fetched, "new")
	if err != nil {
		t.Fatal(err)
	}
	if edited.fetchedVersion != 5 || fmt.Sprint(edited.meta) != "map[importance:0.8 tags:adr,decision type:Decision]" {
		t.Errorf("edited = %+v", edited)
	}

	// The fake enforces versions and holds none, so its answer is scripted.
	backend := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
	}}
	if out := finishEdit(t.Context(), cliDoc(backend), edited); out.code != 0 {
		t.Fatalf("finishEdit = %+v", out)
	}
	if got := backend.PublishCalls[0]; got.ExpectedVersion != 5 || got.Metadata["tags"] != "adr,decision" {
		t.Errorf("publish = %+v, want the version checked and the tags kept", got)
	}

	created, err := editedFrom(protocol.Response{Status: protocol.StatusNotFound}, "new")
	if err != nil || created.fetchedVersion != 0 {
		t.Errorf("new document = %+v, %v, want create only", created, err)
	}
	// An edit is version checked; a document with no version cannot be edited safely.
	if _, err := editedFrom(protocol.Response{Status: protocol.StatusOK, Body: "old"}, "new"); err == nil {
		t.Error("a document without a version was accepted")
	}
}

func TestARefusedPublishPrintsTheServersReason(t *testing.T) {
	const why = "\n# Bad Request\n\npolicy: missing-tags\n"
	backend := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: why}}, nil
	}}
	got, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbPublish, doc: cliDoc(backend), body: "x", expectedVersion: new(1)})
	if err != nil || got.code != 1 || got.status != protocol.StatusBadRequest || got.stdout != why {
		t.Errorf("outcome = %+v, %v, want the reason on stdout", got, err)
	}
}

// A landed archive is a success whoever reports it: the server or the head.
func TestArchiveExitsZero(t *testing.T) {
	answered, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbArchive, doc: cliDoc(&fetchtest.Client{})})
	if err != nil || answered.code != 0 {
		t.Fatalf("answered = %+v, %v", answered, err)
	}
	backend := &fetchtest.Client{
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) {
			return fetch.Result{}, fetchtest.LostResponse()
		},
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetchtest.Archived(), nil
		},
	}
	got, err := runWrite(t.Context(), &cliWrite{verb: protocol.VerbArchive, doc: cliDoc(backend)})
	if err != nil || got.code != 0 || got.metadata["reconciled"] != "true" || got.metadata["archived"] != "true" {
		t.Fatalf("reconciled = %+v, %v", got, err)
	}
	// The archived head carries no version, so the outcome invents none.
	if version, claimed := got.metadata["version"]; claimed {
		t.Errorf("version = %q, want none claimed", version)
	}
}
