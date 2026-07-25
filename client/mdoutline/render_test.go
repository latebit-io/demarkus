package mdoutline

import (
	"strings"
	"testing"
)

func TestBinaryBody(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"markdown", "# Doc\n\nText.\n", false},
		{"empty", "", false},
		{"multibyte utf8", "héllo — ✓\n", false},
		{"png magic", "\x89PNG\r\n\x1a\n\xff\xfe\x00", true},
		{"lone continuation byte", "ok\xb1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BinaryBody(tt.body); got != tt.want {
				t.Errorf("BinaryBody(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestNonMarkdownNotice(t *testing.T) {
	notice := NonMarkdownNotice(42)
	if !strings.Contains(notice, "42 bytes") {
		t.Errorf("notice should carry the byte count, got: %s", notice)
	}
	if !strings.Contains(notice, "non-markdown or binary document") {
		t.Errorf("notice missing the identifying phrase, got: %s", notice)
	}
}
