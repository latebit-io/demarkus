package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupConfigHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "")
	dir := filepath.Join(home, ".demarkus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLocalMemoryAliasResolvesBrandedServer(t *testing.T) {
	branded := "mcp__plugin_acme-brain_memory__mark_publish"
	setupConfigHome(t, map[string]string{"knowledge-systems": "corp\n"})
	if id, err := MemoryTargetID(branded); err != nil || id != "" {
		t.Fatalf("no alias: id=%q err=%v", id, err)
	}
	setupConfigHome(t, map[string]string{"knowledge-systems": "corp\n", LocalMemoryAliasFile: "0123abcd memory\n"})
	if alias, token, err := LocalMemoryAliasRecord(); err != nil || alias != "memory" || token != "0123abcd" {
		t.Fatalf("record = %q %q %v", alias, token, err)
	}
	for _, tool := range []string{branded, "mcp__memory__mark_append", "memory_mark_publish", "mcp__plugin_demarkus-memory_demarkus-memory__mark_publish"} {
		id, err := MemoryTargetID(tool)
		if err != nil || id != LocalMemoryID {
			t.Fatalf("%s: id=%q err=%v", tool, id, err)
		}
		scope, err := KnowledgeScope(tool)
		if err != nil || scope != "" {
			t.Fatalf("%s: knowledge scope=%q err=%v", tool, scope, err)
		}
	}
	if scope, err := KnowledgeScope("mcp__corp__mark_publish"); err != nil || scope != "corp" {
		t.Fatalf("knowledge tool: scope=%q err=%v", scope, err)
	}
	// Catalog reservation shares the routing predicate: acme-memory would be
	// read as plugin "acme" serving "memory".
	for slug, want := range map[string]bool{"memory": true, "demarkus_memory": true, "acme-memory": true, "corp": false} {
		if got, err := ResolvesToLocalMemory(slug); err != nil || got != want {
			t.Fatalf("ResolvesToLocalMemory(%q) = %v, %v", slug, got, err)
		}
	}
	setupConfigHome(t, map[string]string{LocalMemoryAliasFile: "memory\n"})
	if alias, token, err := LocalMemoryAliasRecord(); err != nil || alias != "memory" || token != "" {
		t.Fatalf("token-less record = %q %q %v", alias, token, err)
	}
}

func TestKnowledgeStrictness(t *testing.T) {
	if got := KnowledgeStrictness(); got != Warn {
		t.Fatalf("default = %q, want warn", got)
	}
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "block")
	if got := KnowledgeStrictness(); got != Block {
		t.Fatalf("override = %q, want block", got)
	}
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "invalid")
	if got := KnowledgeStrictness(); got != Warn {
		t.Fatalf("invalid override = %q, want warn", got)
	}
}

// TestMemoryRowCodec pins the catalog record shape every reader and writer share.
func TestMemoryRowCodec(t *testing.T) {
	full := MemoryRow{Slug: "soul", Host: "mark://soul.example", Insecure: true, TokenFile: "/t"}
	if got := full.Record(); got != "soul\tmark://soul.example\t1\t/t" {
		t.Errorf("Record() = %q", got)
	}
	if got := ParseMemoryRow(full.Record()); got != full {
		t.Errorf("round trip = %+v, want %+v", got, full)
	}
	if got := ParseMemoryRow("bare"); got != (MemoryRow{Slug: "bare"}) {
		t.Errorf("short record = %+v", got)
	}
	if got := ParseMemoryRow("s\thttps://b\t0\t-"); !got.IsBroker() || got.Insecure || got.TokenFile != "-" {
		t.Errorf("broker record = %+v", got)
	}
	setupConfigHome(t, map[string]string{"souls": "# comment\n\n" + full.Record() + "\n" + "team\thttps://b\t0\t-\n"})
	slugs, err := ListRemoteMemories()
	if err != nil || strings.Join(slugs, ",") != "soul,team" {
		t.Errorf("ListRemoteMemories = %v, %v", slugs, err)
	}
	rows, err := RemoteMemoryRows()
	if err != nil || len(rows) != 2 || rows[0] != full {
		t.Errorf("RemoteMemoryRows = %+v, %v", rows, err)
	}
}

func TestSaveConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// A space and a quote exercise the shell quoting unquoteShell reverses.
	for _, memory := range []string{"/plain/path", "/path with space", "/path/with'quote", "/Users/x/.demarkus/memory"} {
		if err := SaveConfig(memory, 16312, "isolated", ""); err != nil {
			t.Fatalf("SaveConfig(%q): %v", memory, err)
		}
		cfg, err := LoadConfig()
		if err != nil || cfg == nil {
			t.Fatalf("LoadConfig(%q): %v", memory, err)
		}
		if cfg.MemoryDir != memory || cfg.Port != "16312" || cfg.Mode != "isolated" || cfg.TokensTOML != "" {
			t.Errorf("round trip %q -> %+v", memory, cfg)
		}
	}
	if err := SaveConfig("/root", 6310, "reuse", "/etc/tokens.toml"); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil || cfg == nil || cfg.TokensTOML != "/etc/tokens.toml" || cfg.Mode != "reuse" {
		t.Fatalf("reuse config = %+v, %v", cfg, err)
	}
}
