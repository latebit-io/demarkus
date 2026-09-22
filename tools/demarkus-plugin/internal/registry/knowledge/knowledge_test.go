package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func TestPolicyMirror(t *testing.T) {
	home := registrytest.SetupHome(t)
	// MirrorPolicy only writes for a registered slug, so register first.
	if err := Register("acme"); err != nil {
		t.Fatal(err)
	}
	body := "strictness: block\nrequire_tags: category, team\nrequire_fields: type, authors\n" +
		"strictness: ask\nrequire_tags: ignored\nrequire_fields: ignored\n"
	if err := MirrorPolicy("acme", body); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(home, ".demarkus", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read("plugin-knowledge.strictness.acme") != "block\n" {
		t.Errorf("strictness mirror wrong: %q", read("plugin-knowledge.strictness.acme"))
	}
	if read("plugin-knowledge.require-tags.acme") != "category team\n" {
		t.Errorf("require_tags mirror wrong: %q", read("plugin-knowledge.require-tags.acme"))
	}
	if read("plugin-knowledge.require-fields.acme") != "type authors\n" {
		t.Errorf("require_fields mirror wrong: %q", read("plugin-knowledge.require-fields.acme"))
	}
	if snapshot := read("plugin-knowledge.policy.acme"); !strings.Contains(snapshot, "strictness: block\n") ||
		!strings.Contains(snapshot, "require_tags: category team\n") ||
		!strings.Contains(snapshot, "require_fields: type authors\n") {
		t.Errorf("atomic policy snapshot wrong: %q", snapshot)
	}
	// a knob absent from a later policy clears its file
	if err := MirrorPolicy("acme", "strictness: warn\n"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"plugin-knowledge.require-tags.acme", "plugin-knowledge.require-fields.acme"} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared when absent from policy", name)
		}
	}
	if err := MirrorPolicy("acme", "strictness: invalid\nrequire_tags: category\nrequire_fields: type\n"); err == nil {
		t.Fatal("invalid policy mirror succeeded")
	}
	if read("plugin-knowledge.strictness.acme") != "warn\n" {
		t.Errorf("failed mirror changed strictness: %q", read("plugin-knowledge.strictness.acme"))
	}
	blankFirst := "strictness:\nstrictness: block\nrequire_tags:\nrequire_tags: category\n" +
		"require_fields:\nrequire_fields: type\n"
	if err := MirrorPolicy("acme", blankFirst); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"plugin-knowledge.strictness.acme",
		"plugin-knowledge.require-tags.acme",
		"plugin-knowledge.require-fields.acme",
	} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared by a blank first directive", name)
		}
	}
	if snapshot := read("plugin-knowledge.policy.acme"); !strings.Contains(snapshot, "Atomic mirrored publish policy") {
		t.Errorf("empty atomic snapshot missing: %q", snapshot)
	}
	// Mirroring an UNregistered slug writes nothing (a queued mirror after an
	// unregister must not resurrect policy files).
	if err := MirrorPolicy("ghost", "strictness: block\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-knowledge.strictness.ghost")); !os.IsNotExist(err) {
		t.Error("policy for an unregistered slug should not be written")
	}
}

func TestPolicyMirrorRollsBack(t *testing.T) {
	registrytest.SetupHome(t)
	if err := Register("acme"); err != nil {
		t.Fatal(err)
	}
	if err := MirrorPolicy("acme", "strictness: block\nrequire_tags: category\nrequire_fields: type\n"); err != nil {
		t.Fatal(err)
	}
	registrytest.FailWritesTo(t, "plugin-knowledge.require-tags.acme")
	if err := MirrorPolicy("acme", "strictness: ask\nrequire_tags: team\nrequire_fields: owner\n"); err == nil {
		t.Fatal("policy mirror succeeded despite injected failure")
	}
	policy, err := config.KnowledgePolicy("acme")
	if err != nil {
		t.Fatal(err)
	}
	if policy.Strictness != "block" || strings.Join(policy.RequiredTagAxes, ",") != "category" ||
		strings.Join(policy.RequiredFields, ",") != "type" {
		t.Fatalf("policy after rollback = %#v", policy)
	}
}

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
	// mirror a policy so unregister can prove it clears the per-slug files
	if err := MirrorPolicy("acme", "strictness: block\nrequire_tags: category\nrequire_fields: type\n"); err != nil {
		t.Fatal(err)
	}
	existed, err := Unregister("acme")
	if err != nil || !existed {
		t.Fatalf("unregister acme: existed=%v err=%v", existed, err)
	}
	systems, _ := config.ListKnowledgeSystems()
	if len(systems) != 1 || systems[0] != "beta" {
		t.Fatalf("after unregister want [beta], got %v", systems)
	}
	for _, name := range []string{
		"plugin-knowledge.policy.acme",
		"plugin-knowledge.strictness.acme",
		"plugin-knowledge.require-tags.acme",
		"plugin-knowledge.require-fields.acme",
	} {
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
