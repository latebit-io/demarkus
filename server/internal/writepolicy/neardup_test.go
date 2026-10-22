package writepolicy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/storetest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

func TestEnforceWarnsNearDuplicate(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	tags := map[string]string{"tags": "store"}
	lesson := []byte(neardtest.LessonDoc)
	reworded := []byte(neardtest.RewordedDoc)
	if warnings, err := write(t, gated, "/notes/shards.md", 0, lesson, tags); err != nil || len(warnings) != 0 {
		t.Fatalf("first lesson = %q, %v; want clean", warnings, err)
	}

	t.Run("a reworded copy lands with the warning", func(t *testing.T) {
		warnings, err := write(t, gated, "/notes/again.md", 0, reworded, tags)
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "near-duplicate check: near-duplicate of /notes/shards.md (0.") || !strings.HasSuffix(warnings[0], "; fix: keep one document, append to or link the existing one") {
			t.Errorf("warnings = %q", warnings)
		}
	})
	t.Run("an update of the original is not its own duplicate", func(t *testing.T) {
		warnings, err := write(t, gated, "/notes/shards.md", 1, reworded, tags)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "/notes/shards.md") {
				t.Errorf("own path named: %q", warnings)
			}
		}
	})
	t.Run("the journal is exempt on both sides", func(t *testing.T) {
		if warnings, err := write(t, gated, "/journal/2026-10-21.md", 0, reworded, tags); err != nil || len(warnings) != 0 {
			t.Errorf("journal entry = %q, %v; want clean", warnings, err)
		}
		warnings, err := write(t, gated, "/notes/third.md", 0, reworded, tags)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range warnings {
			if strings.Contains(w, "/journal/") {
				t.Errorf("journal entry named as a target: %q", warnings)
			}
		}
	})
	t.Run("the template names the journal; archived, the default does", func(t *testing.T) {
		direct := backendtest.Direct{Store: raw}
		if _, err := direct.WriteVersion(layout.DocumentPath, 0, []byte("# Template\n\njournal: log/\n"), map[string]string{"tags": "layout"}); err != nil {
			t.Fatal(err)
		}
		if warnings, err := write(t, gated, "/log/2026-10-22.md", 0, reworded, tags); err != nil || len(warnings) != 0 {
			t.Errorf("entry under the template's journal = %q, %v; want clean", warnings, err)
		}
		warnings, err := write(t, gated, "/journal/2026-10-22.md", 0, reworded, tags)
		if err != nil || len(warnings) != 1 {
			t.Errorf("entry under the default journal with another named = %q, %v; want the warning", warnings, err)
		}
		if _, err := direct.Archive(layout.DocumentPath, true); err != nil {
			t.Fatal(err)
		}
		if warnings, err := write(t, gated, "/journal/2026-10-23.md", 0, reworded, tags); err != nil || len(warnings) != 0 {
			t.Errorf("entry under the default journal with the template archived = %q, %v; want clean", warnings, err)
		}
	})
	t.Run("an appended fragment already held elsewhere is named", func(t *testing.T) {
		if warnings, err := write(t, gated, "/notes/tenants.md", 0, []byte(neardtest.OtherDoc), tags); err != nil || len(warnings) != 0 {
			t.Fatalf("tenants = %q, %v; want clean", warnings, err)
		}
		var warnings []string
		_, err := gated.Append(context.Background(), backend.WriteRequest{Path: "/notes/tenants.md", ExpectedVersion: 1, Content: []byte("\n## Shards\n\n" + neardtest.Reworded + "\n"), Warnings: &warnings})
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 1 || !strings.Contains(warnings[0], "near-duplicate of /notes/shards.md") {
			t.Errorf("append warnings = %q", warnings)
		}
	})
	t.Run("no sink, no scan", func(t *testing.T) {
		if _, err := (backendtest.Direct{Store: gated}).WriteVersion("/notes/fourth.md", 0, reworded, tags); err != nil {
			t.Error(err)
		}
	})
	t.Run("a store without a section index is clean", func(t *testing.T) {
		warnings, err := write(t, writepolicy.Enforce(storetest.FallbackStore{DocumentStore: raw}, writepolicy.Options{}), "/notes/fifth.md", 0, reworded, tags)
		if err != nil || len(warnings) != 0 {
			t.Errorf("without sections = %q, %v; want clean", warnings, err)
		}
	})
}

// warmable is a store that reports whether the gate asked it to warm its
// section index at Enforce.
type warmable struct {
	backend.Store
	warmed bool
}

func (w *warmable) WarmSections() { w.warmed = true }

func TestEnforceWarmsTheSectionIndex(t *testing.T) {
	w := &warmable{Store: newStore(t)}
	writepolicy.Enforce(w, writepolicy.Options{})
	if !w.warmed {
		t.Error("Enforce did not warm the store's section index")
	}
}
