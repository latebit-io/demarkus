package protocol

import (
	"strings"
	"testing"
)

func TestIsWorldName(t *testing.T) {
	for _, name := range []string{"root", "latebit", "a", "team-a", "w0", strings.Repeat("a", 63)} {
		if !IsWorldName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "Root", "team_a", "-a", "a-", "a.b", strings.Repeat("a", 64)} {
		if IsWorldName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
