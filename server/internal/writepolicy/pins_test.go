package writepolicy_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

func TestEnforceNotesSourceDrift(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	tags := map[string]string{"tags": "adr"}
	for version := range 3 {
		if _, err := write(t, gated, "/adr/0006.md", version, []byte("# ADR 6\n\nRevision "+strconv.Itoa(version)+".\n"), tags); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a pin behind its source is noted", func(t *testing.T) {
		meta := map[string]string{"tags": "arch", "rel-based-on": "../adr/0006.md/v2, /adr/0006.md/v3, /adr/missing.md/v1, /adr/0006.md/v9, https://github.com/o/r/commit/abcdef0"}
		warnings, err := write(t, gated, "/arch/reader.md", 0, []byte("# Reader\n\nBased on ADR 6.\n"), meta)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"rel-based-on: /adr/0006.md is at v3, past the pinned v2; read what changed, update, pin v3",
			"rel-based-on: /adr/missing.md is not a document here",
			"rel-based-on: /adr/0006.md has no v9; it is at v3; pin the version it was written from",
		}
		if !slices.Equal(warnings, want) {
			t.Errorf("warnings = %q\nwant %q", warnings, want)
		}
	})
	t.Run("a write names the documents pinned to it", func(t *testing.T) {
		warnings, err := write(t, gated, "/adr/0006.md", 3, []byte("# ADR 6\n\nA new requirement.\n"), tags)
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || warnings[0] != "based on an earlier version of this document: /arch/reader.md; check them against this change and re-pin" {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("a document without pins on it hears nothing", func(t *testing.T) {
		if warnings, err := write(t, gated, "/arch/reader.md", 1, []byte("# Reader\n\nEdited.\n"), map[string]string{"tags": "arch", "rel-based-on": "/adr/0006.md/v4"}); err != nil || len(warnings) != 0 {
			t.Errorf("current pin = %q, %v; want clean", warnings, err)
		}
	})
}

func TestEnforceBlocksUnpinnedSource(t *testing.T) {
	raw := newStore(t)
	if _, err := (backendtest.Direct{Store: raw}).WriteVersion(publishpolicy.DocumentPath, 0, []byte(blockUntagged), policyMeta); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	gated := writepolicy.Enforce(raw, writepolicy.Options{Require: true})
	_, err := write(t, gated, "/arch/reader.md", 0, []byte("# Reader\n"), map[string]string{"tags": "domain:x", "rel-based-on": "/adr/0006.md"})
	var policyErr *writepolicy.PolicyError
	if !errors.As(err, &policyErr) || !strings.Contains(policyErr.RejectionMessage(), "unpinned-source: /adr/0006.md") {
		t.Fatalf("unpinned publish = %v, want an unpinned-source refusal", err)
	}
}

func TestPinNotesNameOnlyReadableDocuments(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	tags := map[string]string{"tags": "x"}
	for _, path := range []string{"/private/src.md", "/public/src.md"} {
		for version := range 2 {
			if _, err := write(t, gated, path, version, []byte("# Src\n\nRevision "+strconv.Itoa(version)+".\n"), tags); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := write(t, gated, "/private/dep.md", 0, []byte("# Dep\n"), map[string]string{"tags": "x", "rel-based-on": "/public/src.md/v1"}); err != nil {
		t.Fatal(err)
	}
	readable := func(p string) bool { return !strings.HasPrefix(p, "/private/") }
	publish := func(path string, version int, meta map[string]string) []string {
		var warnings []string
		if _, err := gated.Publish(t.Context(), backend.WriteRequest{Path: path, ExpectedVersion: version, Content: []byte("# Doc\n\nNew.\n"), Metadata: meta, Warnings: &warnings, Readable: readable}); err != nil {
			t.Fatal(err)
		}
		return warnings
	}
	// A pin on a protected source says nothing about it.
	if warnings := publish("/public/probe.md", 0, map[string]string{"tags": "x", "rel-based-on": "/private/src.md/v1, /private/none.md/v1"}); len(warnings) != 0 {
		t.Errorf("pins on protected sources = %q, want no notes", warnings)
	}
	// A protected dependent is not named.
	if warnings := publish("/public/src.md", 2, tags); len(warnings) != 0 {
		t.Errorf("protected dependent = %q, want no notes", warnings)
	}
	// Without the check, both are named.
	if warnings, err := write(t, gated, "/public/control.md", 0, []byte("# C\n"), map[string]string{"tags": "x", "rel-based-on": "/private/src.md/v1"}); err != nil || len(warnings) != 1 {
		t.Errorf("control pin = %q, %v; want the drift note", warnings, err)
	}
	if warnings, err := write(t, gated, "/public/src.md", 3, []byte("# Src\n\nAgain.\n"), tags); err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "/private/dep.md") {
		t.Errorf("control dependents = %q, %v; want /private/dep.md named", warnings, err)
	}
}

// An append carries the base version's pins; its notes judge those too.
func TestAppendNotesAnInheritedStalePin(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	tags := map[string]string{"tags": "x"}
	if _, err := write(t, gated, "/src.md", 0, []byte("# Src\n"), tags); err != nil {
		t.Fatal(err)
	}
	if _, err := write(t, gated, "/notes.md", 0, []byte("# Notes\n"), map[string]string{"tags": "x", "rel-based-on": "/src.md/v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := write(t, gated, "/src.md", 1, []byte("# Src\n\nChanged.\n"), tags); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	if _, err := gated.Append(t.Context(), backend.WriteRequest{Path: "/notes.md", ExpectedVersion: 1, Content: []byte("More.\n"), Warnings: &warnings}); err != nil {
		t.Fatal(err)
	}
	want := []string{"rel-based-on: /src.md is at v2, past the pinned v1; read what changed, update, pin v2"}
	if !slices.Equal(warnings, want) {
		t.Errorf("append warnings = %q, want %q", warnings, want)
	}
}

// Unreadable dependents ranked first never crowd a readable one out of the note.
func TestDependentsNoteSkipsUnreadableBeforeTheCap(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	if _, err := write(t, gated, "/src.md", 0, []byte("# Src\n"), map[string]string{"tags": "x"}); err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		meta := map[string]string{"tags": "x", "importance": "0.9", "rel-based-on": "/src.md/v1"}
		if _, err := write(t, gated, "/private/dep"+strconv.Itoa(i)+".md", 0, []byte("# Dep "+strconv.Itoa(i)+"\n"), meta); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := write(t, gated, "/public/dep.md", 0, []byte("# Public dep\n"), map[string]string{"tags": "x", "importance": "0.1", "rel-based-on": "/src.md/v1"}); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	readable := func(p string) bool { return !strings.HasPrefix(p, "/private/") }
	if _, err := gated.Publish(t.Context(), backend.WriteRequest{Path: "/src.md", ExpectedVersion: 1, Content: []byte("# Src\n\nChanged.\n"), Metadata: map[string]string{"tags": "x"}, Warnings: &warnings, Readable: readable}); err != nil {
		t.Fatal(err)
	}
	want := []string{"based on an earlier version of this document: /public/dep.md; check them against this change and re-pin"}
	if !slices.Equal(warnings, want) {
		t.Errorf("warnings = %q, want %q", warnings, want)
	}
}
