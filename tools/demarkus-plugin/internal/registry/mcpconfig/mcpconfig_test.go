package mcpconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/host"
)

// The MCP config is the user's file: a rewrite must not widen its mode or
// replace a symlinked config with a regular file.
func TestSaveMcpPreservesModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "dotfiles", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(realPath, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "mcp.json")
	if err := os.Symlink(realPath, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := saveMcp(link, map[string]any{"mcpServers": map[string]any{"x": map[string]any{"command": "y"}}}); err != nil {
		t.Fatalf("saveMcp: %v", err)
	}

	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Error("symlinked config was replaced by a regular file")
	}
	realInfo, err := os.Stat(realPath)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := realInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want the original 0600", got)
	}
	data, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Contains(data, []byte(`"x"`)) {
		t.Errorf("target was not rewritten: %q", data)
	}
}

func TestSaveMcpNewFileDefaultsTo0644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := saveMcp(path, map[string]any{"mcpServers": map[string]any{}}); err != nil {
		t.Fatalf("saveMcp: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 0644 for a new config", got)
	}
}

func TestMcpAddRemoveList(t *testing.T) {
	registrytest.SetupHome(t)
	if err := Add(host.Pi, "foo", "bash", []string{"/x.sh"}); err != nil {
		t.Fatal(err)
	}
	if err := AddHTTP(host.Pi, "kb", "https://b/mcp"); err != nil {
		t.Fatal(err)
	}
	names, _ := List(host.Pi)
	if len(names) != 2 {
		t.Fatalf("want 2 servers, got %v", names)
	}
	existed, _ := Remove(host.Pi, "foo")
	if !existed {
		t.Error("foo should have existed")
	}
	existed, _ = Remove(host.Pi, "nope")
	if existed {
		t.Error("nope should not exist")
	}
}

func TestMcpRejectsArrayConfig(t *testing.T) {
	home := registrytest.SetupHome(t)
	cfgDir := filepath.Join(home, ".config", "mcp")
	_ = os.MkdirAll(cfgDir, 0o755)
	cfg := filepath.Join(cfgDir, "mcp.json")
	_ = os.WriteFile(cfg, []byte("[]"), 0o644)
	// A malformed (array) config must error, NOT be silently reset + written back.
	if err := Add(host.Pi, "foo", "bash", nil); err == nil {
		t.Fatal("array-shaped config should error, not silently reset")
	}
	if b, _ := os.ReadFile(cfg); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("malformed config must be left untouched, got %q", string(b))
	}
}

func TestMcpCursorHarnessPathAndShape(t *testing.T) {
	home := registrytest.SetupHome(t)
	if err := Add(host.Cursor, "mem", "/bin/x", []string{"mcp-serve"}); err != nil {
		t.Fatal(err)
	}
	if err := AddHTTP(host.Cursor, "kb", "https://b/mcp"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".cursor", "mcp.json"))
	if err != nil {
		t.Fatalf("cursor config not written: %v", err)
	}
	if strings.Contains(string(b), "\"auth\"") {
		t.Fatalf("cursor HTTP entry must not carry an auth key: %s", b)
	}
	if !strings.Contains(string(b), "\"url\": \"https://b/mcp\"") {
		t.Fatalf("cursor HTTP entry missing url: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "mcp", "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("pi config must be untouched under the cursor harness: %v", err)
	}
}
