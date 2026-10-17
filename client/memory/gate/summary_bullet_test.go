package gate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummary(t *testing.T) {
	title, summary := Summary("# QUIC retries\n\nBack off from 200ms,\ncapped at 5s.\n\nSecond paragraph.\n\n## More\n")
	if title != "QUIC retries" || summary != "Back off from 200ms, capped at 5s." {
		t.Errorf("Summary = %q, %q", title, summary)
	}
	if title, summary := Summary("no heading\n"); title != "" || summary != "" {
		t.Errorf("without an H1 = %q, %q", title, summary)
	}
}

func TestBulletSummary(t *testing.T) {
	if got := BulletSummary("Doc", "What it  settles."); got != "What it settles." {
		t.Errorf("plain = %q", got)
	}
	for _, marked := range []string{"Status: done", "Shipped 2026-10-15", "Merged in #592"} {
		if got := BulletSummary("Doc", marked); got != "" {
			t.Errorf("status marker %q kept: %q", marked, got)
		}
	}
	long := strings.Repeat("word ", 60)
	got := BulletSummary("Doc", long)
	if n := utf8.RuneCountInString("Doc: " + got); n > HubBulletMax || !strings.HasSuffix(got, "word") || strings.Contains(got, "  ") {
		t.Errorf("clipped = %q (%d runes)", got, n)
	}
	if got := BulletSummary(strings.Repeat("t", HubBulletMax), "summary"); got != "" {
		t.Errorf("no room = %q", got)
	}
}
