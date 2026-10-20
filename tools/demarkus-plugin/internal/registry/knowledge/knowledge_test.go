package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func TestKnowledgeRegisterUnregister(t *testing.T) {
	home := registrytest.SetupHome(t)
	if err := Register("acme", "https://acme.example"); err != nil {
		t.Fatal(err)
	}
	if err := Register("beta", ""); err != nil {
		t.Fatal(err)
	}
	// register is idempotent
	if err := Register("acme", "https://acme.example"); err != nil {
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
	if u, err := URL("acme"); err != nil || u != "" {
		t.Fatalf("url after unregister = %q, %v", u, err)
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

func TestClearPolicyMirrorDropsOlderFiles(t *testing.T) {
	home := registrytest.SetupHome(t)
	for _, name := range policyFiles("acme") {
		if err := os.WriteFile(filepath.Join(home, ".demarkus", name), []byte("block\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ClearPolicyMirror("acme"); err != nil {
		t.Fatal(err)
	}
	for _, name := range policyFiles("acme") {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone", name)
		}
	}
	if err := ClearPolicyMirror("bad/slug"); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

func TestRegisterRecordsTheBrokerURL(t *testing.T) {
	registrytest.SetupHome(t)
	if err := Register("acme", ""); err != nil {
		t.Fatal(err)
	}
	if u, err := URL("acme"); err != nil || u != "" {
		t.Fatalf("url of a join without one = %q, %v", u, err)
	}
	// A re-join records the url and repeats idempotently; another url for
	// the same slug is refused until the slug is unregistered.
	if err := Register("acme", "https://acme.example"); err != nil {
		t.Fatal(err)
	}
	if err := Register("acme", "https://acme.example"); err != nil {
		t.Fatal(err)
	}
	if err := Register("acme", "https://acme.example:8443"); err == nil || !strings.Contains(err.Error(), "joined at https://acme.example") {
		t.Fatalf("different url accepted: %v", err)
	}
	if got, _ := URL("acme"); got != "https://acme.example" {
		t.Fatalf("url after refusal = %q", got)
	}
	if err := Register("beta", "https://beta.example"); err != nil {
		t.Fatal(err)
	}
	if got, _ := URL("beta"); got != "https://beta.example" {
		t.Fatalf("beta url = %q", got)
	}
	if systems, _ := config.ListKnowledgeSystems(); len(systems) != 2 {
		t.Fatalf("systems = %v", systems)
	}
}

func TestRegisterRefusesABadURLBeforeWriting(t *testing.T) {
	registrytest.SetupHome(t)
	for _, u := range []string{"acme.example", "https://a\tb", "https://x\nhttps://y", "://nope"} {
		if err := Register("acme", u); err == nil || !strings.Contains(err.Error(), "'acme'") {
			t.Errorf("%q accepted: %v", u, err)
		}
	}
	if systems, _ := config.ListKnowledgeSystems(); len(systems) != 0 {
		t.Fatalf("slug stored despite a refused url: %v", systems)
	}
}

// A failed url write must not leave the slug registered.
func TestRegisterIsAtomicAcrossBothFiles(t *testing.T) {
	registrytest.SetupHome(t)
	registrytest.FailWritesTo(t, urlsFile)
	if err := Register("acme", "https://acme.example"); err == nil {
		t.Fatal("register succeeded with the url write failing")
	}
	if systems, _ := config.ListKnowledgeSystems(); len(systems) != 0 {
		t.Fatalf("slug stored after a failed url write: %v", systems)
	}
}
