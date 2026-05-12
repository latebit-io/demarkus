package broker

import (
	"strings"
	"testing"
)

func TestNewLabel(t *testing.T) {
	// 2^32 label space means birthday collisions are statistically possible
	// at n=1000 (~1 in 8600). Assert format only; collision-retry behavior
	// is covered deterministically by TestMintLabelCollisionRetries.
	const n = 1000
	for range n {
		l, err := NewLabel()
		if err != nil {
			t.Fatalf("NewLabel: %v", err)
		}
		if !strings.HasPrefix(l, LabelPrefix) {
			t.Errorf("label %q missing prefix %q", l, LabelPrefix)
		}
		if len(l) != len(LabelPrefix)+8 {
			t.Errorf("label %q has unexpected length %d", l, len(l))
		}
	}
}
