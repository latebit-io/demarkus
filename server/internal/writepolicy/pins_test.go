package writepolicy_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
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
