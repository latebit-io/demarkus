package registry

import (
	"os"
	"path/filepath"
	"testing"
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
	cfg := filepath.Join(home, ".config", "mcp")
	_ = os.MkdirAll(cfg, 0o755)
	_ = os.WriteFile(filepath.Join(cfg, "mcp.json"), []byte("[]"), 0o644)
	if err := McpAdd("foo", "bash", nil); err != nil {
		t.Fatal(err)
	}
	names, _ := McpList()
	if len(names) != 1 || names[0] != "foo" {
		t.Fatalf("array config should reset to a valid object with foo, got %v", names)
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
