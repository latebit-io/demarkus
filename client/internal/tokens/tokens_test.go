package tokens

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")

	s, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.Hosts()) != 0 {
		t.Errorf("expected empty store, got %d entries", len(s.Hosts()))
	}
}

func TestLoad_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	data := `["localhost:6309"]
token = "abc123"

["example.com:6309"]
token = "def456"
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := s.Get("localhost:6309"); got != "abc123" {
		t.Errorf("localhost token: got %q, want %q", got, "abc123")
	}
	if got := s.Get("example.com:6309"); got != "def456" {
		t.Errorf("example token: got %q, want %q", got, "def456")
	}
}

func TestLoad_InvalidTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	if err := os.WriteFile(path, []byte("not valid {{{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid TOML")
	}
}

func TestGet_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(filepath.Join(dir, "tokens.toml"))

	if got := s.Get("unknown:6309"); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestSetAndGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	s, _ := Load(path)

	if err := s.Set("localhost:6309", "my-token"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if got := s.Get("localhost:6309"); got != "my-token" {
		t.Errorf("Get after Set: got %q, want %q", got, "my-token")
	}

	// Verify persisted to disk.
	s2, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s2.Get("localhost:6309"); got != "my-token" {
		t.Errorf("Get after reload: got %q, want %q", got, "my-token")
	}
}

func TestSetOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	s, _ := Load(path)

	_ = s.Set("localhost:6309", "old-token")
	_ = s.Set("localhost:6309", "new-token")

	if got := s.Get("localhost:6309"); got != "new-token" {
		t.Errorf("Get after overwrite: got %q, want %q", got, "new-token")
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	s, _ := Load(path)

	_ = s.Set("localhost:6309", "my-token")
	if err := s.Remove("localhost:6309"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if got := s.Get("localhost:6309"); got != "" {
		t.Errorf("Get after Remove: got %q, want empty", got)
	}

	// Verify persisted.
	s2, _ := Load(path)
	if got := s2.Get("localhost:6309"); got != "" {
		t.Errorf("Get after reload: got %q, want empty", got)
	}
}

func TestHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	s, _ := Load(path)

	_ = s.Set("b.com:6309", "tok1")
	_ = s.Set("a.com:6309", "tok2")

	hosts := s.Hosts()
	if len(hosts) != 2 {
		t.Fatalf("hosts count: got %d, want 2", len(hosts))
	}
	if hosts[0] != "a.com:6309" {
		t.Errorf("first host: got %q, want %q", hosts[0], "a.com:6309")
	}
	if hosts[1] != "b.com:6309" {
		t.Errorf("second host: got %q, want %q", hosts[1], "b.com:6309")
	}
}

func TestSave_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "tokens.toml")
	s, _ := Load(path)

	if err := s.Set("localhost:6309", "tok"); err != nil {
		t.Fatalf("Set with nested dir: %v", err)
	}

	// Verify file exists with restrictive permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file permissions: got %o, want 600", info.Mode().Perm())
	}
}
