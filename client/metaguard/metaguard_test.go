package metaguard

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

func TestCompareReportsDroppedTagsAndKeys(t *testing.T) {
	current := map[string]string{
		"version": "7", "etag": "x", "content-hash": "y", "modified": "z", "agent": "a", "retention": "3",
		"tags": "plan, lookup,verbose", "importance": "0.9", "title": "Plan", "type": "Plan",
		"rel-related": "/a.md,/b.md", "source": strings.Repeat("s", 100),
	}
	incoming := map[string]string{"tags": "lookup", "title": "Plan", "agent": "b"}
	n := Compare(current, incoming)
	if got, want := strings.Join(n.Tags, ","), "plan,verbose"; got != want {
		t.Fatalf("tags = %q, want %q", got, want)
	}
	want := []string{"importance=0.9", "rel-related=/a.md,/b.md", "source=" + strings.Repeat("s", 80) + "...", "type=Plan"}
	if got := strings.Join(n.Keys, "|"); got != strings.Join(want, "|") {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	note := n.Note("7")
	for _, sub := range []string{"note: this publish dropped tags plan, verbose and keys importance=0.9;", "carried by v7", "verbose: true"} {
		if !strings.Contains(note, sub) {
			t.Errorf("note missing %q:\n%s", sub, note)
		}
	}
}

func TestCompareNothingDropped(t *testing.T) {
	current := map[string]string{"version": "3", "tags": "a,b", "title": "T", "retention": "2", "type": "Document"}
	for _, incoming := range []map[string]string{
		{"tags": "b, a, c", "title": "T2"},
		{"tags": "a,b", "title": "T", "extra": "new"},
	} {
		if n := Compare(current, incoming); n.Note("3") != "" {
			t.Fatalf("unexpected narrowing %+v for %v", n, incoming)
		}
	}
	if n := Compare(nil, map[string]string{"tags": "a"}); n.Note("3") != "" {
		t.Fatalf("nil current narrowed: %+v", n)
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	value := strings.Repeat("a", 79) + "€"
	n := Compare(map[string]string{"source": value}, map[string]string{})
	if got := n.Keys[0]; got != "source="+strings.Repeat("a", 79)+"..." || !utf8.ValidString(got) {
		t.Fatalf("truncated value = %q", got)
	}
	if n := Compare(map[string]string{"source": strings.Repeat("a", 80)}, map[string]string{}); n.Keys[0] != "source="+strings.Repeat("a", 80) {
		t.Fatalf("value at the cap was truncated: %q", n.Keys[0])
	}
}

func TestNoteWordsEachPartAlone(t *testing.T) {
	if got := (Narrowing{Tags: []string{"a"}}).Note("2"); !strings.Contains(got, "dropped tags a carried by v2") {
		t.Fatalf("tags-only note: %q", got)
	}
	if got := (Narrowing{Keys: []string{"type=Plan"}}).Note("2"); !strings.Contains(got, "dropped keys type=Plan carried by v2") {
		t.Fatalf("keys-only note: %q", got)
	}
}

func TestGateStatuses(t *testing.T) {
	meta := map[string]string{"tags": "a"}
	read := func(status string) func(context.Context) (fetch.Result, error) {
		return func(context.Context) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: status,
				Metadata: map[string]string{"version": "3", "tags": "a,b"}}}, nil
		}
	}
	if note, err := Gate(context.Background(), 3, meta, read(protocol.StatusOK)); err != nil || !strings.Contains(note, "dropped tags b carried by v3") {
		t.Fatalf("ok read: note %q err %v", note, err)
	}
	if note, err := Gate(context.Background(), 3, meta, read(protocol.StatusNotFound)); err != nil || note != "" {
		t.Fatalf("not-found read: note %q err %v", note, err)
	}
	if note, err := Gate(context.Background(), 3, meta, read(protocol.StatusUnauthorized)); err == nil || note != "" {
		t.Fatalf("unauthorized read: note %q err %v", note, err)
	}
	if note, err := Gate(context.Background(), 0, meta, read(protocol.StatusOK)); err != nil || note != "" {
		t.Fatalf("create: note %q err %v", note, err)
	}
}
