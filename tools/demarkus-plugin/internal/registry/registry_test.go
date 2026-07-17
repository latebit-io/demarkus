package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".demarkus"), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestDeriveSlug(t *testing.T) {
	cases := map[string]string{
		"mcp.broker.acme.com": "acme",
		"MCP.BROKER.Acme.com": "acme",
		"broker.acme.com":     "acme",
		"acme-broker.example": "acme-broker",
		"soul.demarkus.io":    "soul",
	}
	for host, want := range cases {
		if got := deriveSlug(host); got != want {
			t.Errorf("deriveSlug(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestMcpAddRemoveList(t *testing.T) {
	setupHome(t)
	if err := McpAdd("foo", "bash", []string{"/x.sh"}); err != nil {
		t.Fatal(err)
	}
	if err := McpAddHTTP("kb", "https://b/mcp"); err != nil {
		t.Fatal(err)
	}
	names, _ := McpList()
	if len(names) != 2 {
		t.Fatalf("want 2 servers, got %v", names)
	}
	existed, _ := McpRemove("foo")
	if !existed {
		t.Error("foo should have existed")
	}
	existed, _ = McpRemove("nope")
	if existed {
		t.Error("nope should not exist")
	}
}

func TestMcpRejectsArrayConfig(t *testing.T) {
	home := setupHome(t)
	cfgDir := filepath.Join(home, ".config", "mcp")
	_ = os.MkdirAll(cfgDir, 0o755)
	cfg := filepath.Join(cfgDir, "mcp.json")
	_ = os.WriteFile(cfg, []byte("[]"), 0o644)
	// A malformed (array) config must error, NOT be silently reset + written back.
	if err := McpAdd("foo", "bash", nil); err == nil {
		t.Fatal("array-shaped config should error, not silently reset")
	}
	if b, _ := os.ReadFile(cfg); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("malformed config must be left untouched, got %q", string(b))
	}
}

func TestSoulJoinAndCollision(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	_ = os.MkdirAll(repo, 0o755)
	res, err := SoulJoin("soul.demarkus.io", "tok", true, repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "soul" {
		t.Fatalf("slug: want soul, got %s", res.Slug)
	}
	// token written 0600
	info, err := os.Stat(res.TokenFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file perms: %v %v", err, info)
	}
	// binding recorded
	if b, _ := IsCatalogSoul("soul"); !b {
		t.Error("soul should be a catalog soul")
	}
	// collision: a different host under the same slug is rejected
	if _, err := SoulJoin("soul.other.net", "t2", false, ""); err == nil {
		t.Error("expected slug collision error for a different host")
	}
	// re-join same host is fine (idempotent upsert)
	if _, err := SoulJoin("soul.demarkus.io", "tok2", true, ""); err != nil {
		t.Errorf("re-join same host should succeed: %v", err)
	}
	// reserved slug rejected
	if _, err := SoulJoin("demarkus-memory.example.com", "", false, ""); err == nil {
		t.Error("expected reserved-slug rejection")
	}
}

func TestPolicyMirror(t *testing.T) {
	home := setupHome(t)
	// PolicyMirror only writes for a registered slug, so register first.
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	body := "strictness: block\nrequire_tags: category team\nrequire_fields: type\n\nsome prose strictness: ignored\n"
	if err := PolicyMirror("acme", body); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(home, ".demarkus", name))
		return string(b)
	}
	if read("plugin-knowledge.strictness.acme") != "block\n" {
		t.Errorf("strictness mirror wrong: %q", read("plugin-knowledge.strictness.acme"))
	}
	if read("plugin-knowledge.require-tags.acme") != "category team\n" {
		t.Errorf("require_tags mirror wrong: %q", read("plugin-knowledge.require-tags.acme"))
	}
	// a knob absent from a later policy clears its file
	if err := PolicyMirror("acme", "strictness: warn\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-knowledge.require-tags.acme")); !os.IsNotExist(err) {
		t.Error("require_tags file should be cleared when absent from policy")
	}
	// Mirroring an UNregistered slug writes nothing (a queued mirror after an
	// unregister must not resurrect policy files).
	if err := PolicyMirror("ghost", "strictness: block\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-knowledge.strictness.ghost")); !os.IsNotExist(err) {
		t.Error("policy for an unregistered slug should not be written")
	}
}

func TestKnowledgeRegisterUnregister(t *testing.T) {
	home := setupHome(t)
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	if err := KnowledgeRegister("beta"); err != nil {
		t.Fatal(err)
	}
	// register is idempotent
	if err := KnowledgeRegister("acme"); err != nil {
		t.Fatal(err)
	}
	// mirror a policy so unregister can prove it clears the per-slug files
	if err := PolicyMirror("acme", "strictness: block\nrequire_tags: category\n"); err != nil {
		t.Fatal(err)
	}
	existed, err := KnowledgeUnregister("acme")
	if err != nil || !existed {
		t.Fatalf("unregister acme: existed=%v err=%v", existed, err)
	}
	systems, _ := config.ListKnowledgeSystems()
	if len(systems) != 1 || systems[0] != "beta" {
		t.Fatalf("after unregister want [beta], got %v", systems)
	}
	for _, name := range []string{"plugin-knowledge.strictness.acme", "plugin-knowledge.require-tags.acme"} {
		if _, err := os.Stat(filepath.Join(home, ".demarkus", name)); !os.IsNotExist(err) {
			t.Errorf("%s should be cleared on unregister", name)
		}
	}
	// unregistering an unknown slug is a no-op reporting existed=false
	existed, err = KnowledgeUnregister("ghost")
	if err != nil || existed {
		t.Fatalf("unregister ghost: existed=%v err=%v", existed, err)
	}
}

func TestPromoteTargetAdd(t *testing.T) {
	setupHome(t)
	if err := PromoteTargetAdd("acme", "/shared", "Acme shared"); err != nil {
		t.Fatal(err)
	}
	if err := PromoteTargetAdd("acme", "bad", ""); err == nil {
		t.Error("path not starting with / should error")
	}
	rows, _ := PromoteTargetList()
	if len(rows) != 1 || rows[0] != "acme /shared Acme shared" {
		t.Fatalf("promote target row wrong: %v", rows)
	}
}

func TestSoulJoinURLWithFragment(t *testing.T) {
	home := setupHome(t)
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := SoulJoin("mark://kb.example.com#token=fragtok", "", false, repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Slug != "kb" || res.Host != "mark://kb.example.com" {
		t.Fatalf("slug/host: got %s %s", res.Slug, res.Host)
	}
	b, err := os.ReadFile(res.TokenFile)
	if err != nil || string(b) != "fragtok" {
		t.Fatalf("token file: %v %q", err, b)
	}

	row, ok, err := RemoteSoulRow("kb")
	if err != nil || !ok {
		t.Fatalf("RemoteSoulRow: %v ok=%v", err, ok)
	}
	if row.Host != "mark://kb.example.com" || row.Insecure {
		t.Fatalf("row round trip: %+v", row)
	}

	// Conflicting explicit --token and fragment token is rejected.
	if _, err := SoulJoin("mark://kb2.example.com#token=a", "b", false, ""); err == nil {
		t.Error("expected conflict error for --token + fragment token")
	}
	// Bad fragment key fails loudly.
	if _, err := SoulJoin("mark://kb3.example.com#tokn=a", "", false, ""); err == nil {
		t.Error("expected error for unknown fragment key")
	}
	// Bracketed IPv6 would derive a garbage slug; the join-URL path rejects it.
	if _, err := SoulJoin("mark://[2001:db8::1]:6309#token=a", "", false, ""); err == nil {
		t.Error("expected error for bracketed IPv6 join URL")
	}
	// Malformed hosts on the legacy (no-fragment) path fail loudly too.
	for _, bad := range []string{
		"mark://kb.example.com?x=1",
		"mark://user@kb.example.com",
		"mark://kb.example.com/docs",
	} {
		if _, err := SoulJoin(bad, "t", false, ""); err == nil {
			t.Errorf("expected error for malformed host %q", bad)
		}
	}
}
