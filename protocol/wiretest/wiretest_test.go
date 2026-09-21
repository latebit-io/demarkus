package wiretest

import (
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestEveryGoldenParses(t *testing.T) {
	names := Names(t)
	for _, verb := range []string{"fetch", "list", "versions", "lookup", "publish-created", "append", "archive"} {
		if !slices.Contains(names, verb) {
			t.Errorf("no golden named %q; have %v", verb, names)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if resp := Response(t, name); resp.Status == "" {
				t.Error("golden has no status")
			}
		})
	}
	if got := Response(t, "list").Status; got != protocol.StatusOK {
		t.Errorf("list status = %q", got)
	}
}
