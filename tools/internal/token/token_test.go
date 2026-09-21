package token

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
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
				t.Errorf("Hash = %q, want prefix sha256-", m.Entry.Hash)
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

// TestGenerateDefensiveCopy verifies that mutating the slices passed to
// Generate after the call does not corrupt the returned Minted.Entry.
// Without slices.Clone in Generate this test would catch caller aliasing.
func TestGenerateDefensiveCopy(t *testing.T) {
	paths := []string{"/docs/*"}
	ops := []string{"publish"}

	m, err := Generate("label", paths, ops)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	paths[0] = "/mutated/*"
	ops[0] = "delete"

	if m.Entry.Paths[0] != "/docs/*" {
		t.Errorf("Entry.Paths aliased caller slice: got %q, want \"/docs/*\"", m.Entry.Paths[0])
	}
	if m.Entry.Operations[0] != "publish" {
		t.Errorf("Entry.Operations aliased caller slice: got %q, want \"publish\"", m.Entry.Operations[0])
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
