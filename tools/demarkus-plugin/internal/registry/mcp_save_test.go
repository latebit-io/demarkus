package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
