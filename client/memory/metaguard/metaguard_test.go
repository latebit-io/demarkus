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
		"version": "7", "etag": "x", "content-hash": "y", "modified": "z", "agent": "a", "user": "u@example.com", "retention": "3",
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
	warning := n.Warning("7")
	for _, sub := range []string{"this publish dropped tags plan, verbose and keys importance=0.9;", "carried by v7", "force: true"} {
		if !strings.Contains(warning, sub) {
			t.Errorf("warning missing %q:\n%s", sub, warning)
		}
	}
}

func TestCompareNothingDropped(t *testing.T) {
	current := map[string]string{"version": "3", "tags": "a,b", "title": "T", "retention": "2", "type": "Document"}
	for _, incoming := range []map[string]string{
		{"tags": "b, a, c", "title": "T2"},
		{"tags": "a,b", "title": "T", "extra": "new"},
	} {
		if n := Compare(current, incoming); n.Warning("3") != "" {
			t.Fatalf("unexpected narrowing %+v for %v", n, incoming)
		}
	}
	if n := Compare(nil, map[string]string{"tags": "a"}); n.Warning("3") != "" {
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

func TestWarningWordsEachPartAlone(t *testing.T) {
	if got := (Narrowing{Tags: []string{"a"}}).Warning("2"); !strings.Contains(got, "dropped tags a carried by v2") {
		t.Fatalf("tags-only warning: %q", got)
	}
	if got := (Narrowing{Keys: []string{"type=Plan"}}).Warning("2"); !strings.Contains(got, "dropped keys type=Plan carried by v2") {
		t.Fatalf("keys-only warning: %q", got)
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
	if warning, err := Gate(context.Background(), 3, meta, read(protocol.StatusOK)); err != nil || !strings.Contains(warning, "dropped tags b carried by v3") {
		t.Fatalf("ok read: warning %q err %v", warning, err)
	}
	if warning, err := Gate(context.Background(), 3, meta, read(protocol.StatusNotFound)); err != nil || warning != "" {
		t.Fatalf("not-found read: warning %q err %v", warning, err)
	}
	if warning, err := Gate(context.Background(), 3, meta, read(protocol.StatusUnauthorized)); err == nil || warning != "" {
		t.Fatalf("unauthorized read: warning %q err %v", warning, err)
	}
	if warning, err := Gate(context.Background(), 0, meta, read(protocol.StatusOK)); err != nil || warning != "" {
		t.Fatalf("create: warning %q err %v", warning, err)
	}
}
