package storefmt

import (
	"errors"
	"testing"
)

// Committed names every outcome where a version became current, so callers
// catalog and hint it, however the write is reported.
func TestCommittedOutcomes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"clean", nil, true},
		{"not synced", ErrCommittedNotSynced, true},
		{"stale under conflict", errors.Join(ErrConflict, ErrCommittedStale), true},
		{"conflict before the write", ErrConflict, false},
		{"other failure", errors.New("disk full"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Committed(tt.err); got != tt.want {
				t.Fatalf("Committed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
