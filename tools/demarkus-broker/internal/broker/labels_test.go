package broker

import (
	"strings"
	"testing"
)

func TestNewLabel(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
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
		if _, dup := seen[l]; dup {
			t.Errorf("duplicate label %q in %d draws", l, n)
		}
		seen[l] = struct{}{}
	}
}
