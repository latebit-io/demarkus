package catalog

import (
	"testing"
	"time"
)

func TestParseFilterErrors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"single exact", "project=broker", false},
		{"multiple", "project=broker,type=note", false},
		{"tag membership", "tag=go", false},
		{"modified-after date", "modified-after=2025-01-01", false},
		{"modified-before rfc3339", "modified-before=2025-01-01T00:00:00Z", false},
		{"no equals", "project", true},
		{"empty key", "=broker", true},
		{"empty value", "project=", true},
		{"bad date", "modified-after=not-a-date", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseFilter(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseFilter(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestPredicateMatches(t *testing.T) {
	mod := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	e := &Entry{
		Tags:     []string{"go", "auth"},
		Modified: mod,
		Metadata: map[string]string{"project": "broker", "type": "note"},
	}

	tests := []struct {
		name   string
		filter string
		want   bool
	}{
		{"exact match", "project=broker", true},
		{"exact mismatch", "project=universe", false},
		{"exact missing key", "author=fritz", false},
		{"tag present", "tag=go", true},
		{"tag case insensitive", "tag=GO", true},
		{"tag absent", "tag=rust", false},
		{"modified-after true", "modified-after=2025-01-01", true},
		{"modified-after false", "modified-after=2025-12-01", false},
		{"modified-before true", "modified-before=2025-12-01", true},
		{"modified-before false", "modified-before=2025-01-01", false},
		{"all satisfied", "project=broker,tag=auth", true},
		{"one fails", "project=broker,tag=rust", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preds, err := ParseFilter(tt.filter)
			if err != nil {
				t.Fatalf("ParseFilter(%q): %v", tt.filter, err)
			}
			if got := matchesAll(e, preds); got != tt.want {
				t.Errorf("matchesAll for %q = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}
