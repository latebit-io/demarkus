package token

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit/demarkus/protocol"
)

func TestGenerate(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		paths      []string
		operations []string
		wantErr    error
	}{
		{
			name:       "single path single op",
			label:      "fritz-laptop",
			paths:      []string{"/docs/*"},
			operations: []string{"publish"},
		},
		{
			name:       "multi path multi op",
			label:      "team-a",
			paths:      []string{"/team-a/*", "/shared/*"},
			operations: []string{"read", "publish"},
		},
		{
			name:       "empty label rejected",
			label:      "",
			paths:      []string{"/*"},
			operations: []string{"publish"},
			wantErr:    ErrEmptyLabel,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Generate(tt.label, tt.paths, tt.operations)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Generate err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				return
			}
			if m.Label != tt.label {
				t.Errorf("Label = %q, want %q", m.Label, tt.label)
			}
			if len(m.Raw) != 64 {
				t.Errorf("Raw length = %d, want 64", len(m.Raw))
			}
			if !strings.HasPrefix(m.Entry.Hash, "sha256-") {
				t.Errorf("Hash prefix = %q, want sha256-", m.Entry.Hash[:7])
			}
			if m.Entry.Hash != protocol.HashToken(m.Raw) {
				t.Error("Entry.Hash does not match protocol.HashToken(Raw)")
			}
			if len(m.Entry.Paths) != len(tt.paths) {
				t.Errorf("Paths len = %d, want %d", len(m.Entry.Paths), len(tt.paths))
			}
			if len(m.Entry.Operations) != len(tt.operations) {
				t.Errorf("Operations len = %d, want %d", len(m.Entry.Operations), len(tt.operations))
			}
		})
	}
}

func TestGenerateUniqueness(t *testing.T) {
	seen := make(map[string]struct{})
	for range 100 {
		m, err := Generate("label", []string{"/*"}, []string{"publish"})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if _, dup := seen[m.Raw]; dup {
			t.Fatalf("duplicate raw token across 100 mints: %s", m.Raw)
		}
		seen[m.Raw] = struct{}{}
	}
}
