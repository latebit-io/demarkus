package writepolicy_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/layout"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/neardup/neardtest"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/storetest"
	"github.com/latebit-io/demarkus/server/internal/writepolicy"
)

func TestEnforceWarnsNearDuplicate(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Warn}})
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
	t.Run("an override keeps both and says so", func(t *testing.T) {
		meta := map[string]string{"tags": "store", neardup.OverrideKey: " the lesson is quoted on purpose "}
		if warnings, err := write(t, gated, "/notes/kept.md", 0, reworded, meta); err != nil || len(warnings) != 1 || warnings[0] != "near-duplicate check overridden (the lesson is quoted on purpose)" {
			t.Errorf("override = %q, %v; want the reason noted", warnings, err)
		}
	})
	t.Run("no sink, no scan", func(t *testing.T) {
		if _, err := (backendtest.Direct{Store: gated}).WriteVersion("/notes/fourth.md", 0, reworded, tags); err != nil {
			t.Error(err)
		}
	})
	t.Run("a store without a section index lands with the skip noted", func(t *testing.T) {
		cold := readerOnly{storetest.FallbackStore{DocumentStore: raw}}
		warnings, err := write(t, writepolicy.Enforce(cold, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Warn}}), "/notes/fifth.md", 0, reworded, tags)
		if err != nil || len(warnings) != 1 || warnings[0] != "near-duplicate check skipped: no section index covers this write yet" {
			t.Errorf("without sections = %q, %v; want the skip noted", warnings, err)
		}
	})
}

// readerOnly hands the commit a state without a catalog, as the bucket store
// lends its commit attempts.
type readerOnly struct{ backend.Store }

func (r readerOnly) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	inner := req.Precondition
	req.Precondition = func(ctx context.Context, state backend.Reader, write storefmt.PreparedWrite) error {
		return inner(ctx, struct{ backend.Reader }{state}, write)
	}
	return r.Store.Publish(ctx, req)
}

// A check that cannot run refuses the write in block mode and notes it in
// warn mode; the file store judges again inside the commit, so a copy that
// landed after the pre-commit look is still refused.
func TestEnforceNearDuplicateCheckFailures(t *testing.T) {
	raw := newStore(t)
	tags := map[string]string{"tags": "store"}
	if _, err := write(t, raw, "/notes/shards.md", 0, []byte(neardtest.LessonDoc), tags); err != nil {
		t.Fatal(err)
	}
	broken := brokenLookups{raw}
	if _, err := write(t, writepolicy.Enforce(broken, writepolicy.Options{}), "/notes/again.md", 0, []byte(neardtest.RewordedDoc), tags); err == nil || !strings.Contains(err.Error(), "near-duplicate check: index on fire") {
		t.Errorf("block mode with a failing check = %v, want the failure refused", err)
	}
	if v, err := (backendtest.Direct{Store: raw}).CurrentVersion("/notes/again.md"); err != nil || v != 0 {
		t.Errorf("refused write landed at v%d, %v", v, err)
	}
	// Warn mode notes the failed look; the file store's commit judges again
	// and that verdict, the warning, is what lands.
	warnings, err := write(t, writepolicy.Enforce(broken, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Warn}}), "/notes/again.md", 0, []byte(neardtest.RewordedDoc), tags)
	if err != nil || len(warnings) != 1 || !strings.HasPrefix(warnings[0], "near-duplicate check: near-duplicate of /notes/shards.md (0.76 shared)") {
		t.Errorf("warn mode with a failing check = %q, %v; want the commit's warning", warnings, err)
	}
	warnings, err = write(t, writepolicy.Enforce(readerOnly{broken}, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Warn}}), "/notes/fourth.md", 0, []byte(neardtest.RewordedDoc), tags)
	if err != nil || len(warnings) != 1 || warnings[0] != "near-duplicate check skipped: near-duplicate check: index on fire" {
		t.Errorf("warn mode, failing check, no catalog at commit = %q, %v; want the skip noted", warnings, err)
	}
	// The pre-commit look sees a cold index; the commit itself sees the lesson.
	cold := storetest.FallbackStore{DocumentStore: raw}
	if _, err := write(t, writepolicy.Enforce(cold, writepolicy.Options{}), "/notes/third.md", 0, []byte(neardtest.RewordedDoc), tags); !errors.Is(err, writepolicy.ErrNearDuplicate) {
		t.Errorf("commit-time judgment = %v, want the refusal", err)
	}
}

// brokenLookups is a store whose views fail every section-index lookup.
type brokenLookups struct{ backend.Store }

func (b brokenLookups) OpenReadView(ctx context.Context) (backend.ReadView, error) {
	view, err := b.Store.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return brokenView{view}, nil
}

type brokenView struct{ backend.ReadView }

func (v brokenView) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	if opts.Match != catalog.MatchCatalog && opts.Match != "" {
		return nil, errors.New("index on fire")
	}
	return v.ReadView.Lookup(ctx, query, opts)
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

// A bucket store reopened on a world that already holds the lesson judges
// its first gated write: Enforce warms the section index at wrap, and the
// gate's stale read waits for the build instead of passing the write.
func TestFirstWriteAfterReopenIsJudged(t *testing.T) {
	ctx := context.Background()
	memory, err := blob.NewMemory(4 << 20)
	if err != nil {
		t.Fatal(err)
	}
	options := bucketstore.Options{Logger: slog.New(slog.DiscardHandler), WorldID: "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48"}
	tags := map[string]string{"tags": "store"}
	writer, err := bucketstore.Open(ctx, memory, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write(t, writer, "/notes/shards.md", 0, []byte(neardtest.LessonDoc), tags); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	fresh, err := bucketstore.Open(ctx, memory, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = write(t, writepolicy.Enforce(fresh, writepolicy.Options{}), "/notes/again.md", 0, []byte(neardtest.RewordedDoc), tags)
	var refused *writepolicy.NearDuplicateError
	if !errors.As(err, &refused) || len(refused.Hits) != 1 || refused.Hits[0].Path != "/notes/shards.md" {
		t.Errorf("first gated write after reopen = %v, want the near-duplicate refusal naming the lesson", err)
	}
}

// The default mode refuses: the write never lands, the refusal names the
// document and the override; an update of the original, a journal entry
// and an override still land. Off runs nothing.
func TestEnforceRefusesNearDuplicate(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	tags := map[string]string{"tags": "store"}
	if _, err := write(t, gated, "/notes/shards.md", 0, []byte(neardtest.LessonDoc), tags); err != nil {
		t.Fatal(err)
	}
	_, err := write(t, gated, "/notes/again.md", 0, []byte(neardtest.RewordedDoc), tags)
	var refused *writepolicy.NearDuplicateError
	if !errors.Is(err, writepolicy.ErrNearDuplicate) || !errors.Is(err, backend.ErrRejected) || !errors.As(err, &refused) {
		t.Fatalf("reworded copy = %v, want a near-duplicate rejection", err)
	}
	want := "near-duplicate check refused this write: near-duplicate of /notes/shards.md (0.76 shared)\nFix: update or append to that document instead; to keep both, republish with metadata near-duplicate-override set to the reason.\n"
	if got := refused.RejectionMessage(); got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
	direct := backendtest.Direct{Store: raw}
	if v, err := direct.CurrentVersion("/notes/again.md"); err != nil || v != 0 {
		t.Errorf("refused write landed at v%d, %v", v, err)
	}
	if _, err := write(t, gated, "/notes/shards.md", 1, []byte(neardtest.RewordedDoc), tags); err != nil {
		t.Errorf("update of the original = %v, want it to land", err)
	}
	if _, err := write(t, gated, "/journal/2026-10-22.md", 0, []byte(neardtest.RewordedDoc), tags); err != nil {
		t.Errorf("journal entry = %v, want it to land", err)
	}
	meta := map[string]string{"tags": "store", neardup.OverrideKey: "quoted on purpose"}
	if warnings, err := write(t, gated, "/notes/kept.md", 0, []byte(neardtest.RewordedDoc), meta); err != nil || len(warnings) != 1 {
		t.Errorf("override = %q, %v; want it to land with the reason noted", warnings, err)
	}
	off := writepolicy.Enforce(raw, writepolicy.Options{Content: writepolicy.ContentOptions{NearDuplicate: contentcheck.Off}})
	if warnings, err := write(t, off, "/notes/off.md", 0, []byte(neardtest.RewordedDoc), tags); err != nil || len(warnings) != 0 {
		t.Errorf("off = %q, %v; want it to land clean", warnings, err)
	}
}

// A split topic repeats its parent until the hub drops the section, so the
// parent its curated-from names is skipped; any other document still counts.
func TestEnforceSkipsTheSplitParent(t *testing.T) {
	raw := newStore(t)
	gated := writepolicy.Enforce(raw, writepolicy.Options{})
	if _, err := write(t, gated, "/notes/shards.md", 0, []byte(neardtest.LessonDoc), map[string]string{"tags": "store"}); err != nil {
		t.Fatal(err)
	}
	split := map[string]string{"tags": "store", neardup.CuratedFromKey: "notes/shards.md"}
	if warnings, err := write(t, gated, "/notes/shards/lesson.md", 0, []byte(neardtest.LessonDoc), split); err != nil || len(warnings) != 0 {
		t.Errorf("topic of its parent = %q, %v; want it to land clean", warnings, err)
	}
	split[neardup.CuratedFromKey] = "/notes/other.md"
	if _, err := write(t, gated, "/notes/other/lesson.md", 0, []byte(neardtest.LessonDoc), split); !errors.Is(err, writepolicy.ErrNearDuplicate) {
		t.Errorf("topic naming another parent = %v, want the refusal", err)
	}
}
