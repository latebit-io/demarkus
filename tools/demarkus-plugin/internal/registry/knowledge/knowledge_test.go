package knowledge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func TestKnowledgeRegisterUnregister(t *testing.T) {
	home := registrytest.SetupHome(t)
	if err := Register("acme"); err != nil {
		t.Fatal(err)
	}
	if err := Register("beta"); err != nil {
		t.Fatal(err)
	}
	// register is idempotent
	if err := Register("acme"); err != nil {
		t.Fatal(err)
	}
	// files an older helper mirrored; unregister clears them
	for _, name := range policyFiles("acme") {
		if err := os.WriteFile(filepath.Join(home, ".demarkus", name), []byte("block\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	existed, err := Unregister("acme")
	if err != nil || !existed {
		t.Fatalf("unregister acme: existed=%v err=%v", existed, err)
	}
	systems, _ := config.ListKnowledgeSystems()
	if len(systems) != 1 || systems[0] != "beta" {
		t.Fatalf("after unregister want [beta], got %v", systems)
	}
	for _, name := range policyFiles("acme") {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared on unregister", name)
		}
	}
	// unregistering an unknown slug is a no-op reporting existed=false
	existed, err = Unregister("ghost")
	if err != nil || existed {
		t.Fatalf("unregister ghost: existed=%v err=%v", existed, err)
	}
}
