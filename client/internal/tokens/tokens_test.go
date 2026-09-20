package tokens

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Set("localhost:6309", "stored-token"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	const origin = "localhost:6309"
	if err := s.Set("other.example:6309", "other-stored"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	tests := []struct {
		name   string
		cred   Credential
		host   string
		envVal string
		store  *Store
		want   string
	}{
		{"explicit wins", Credential{Explicit: "flag-token", Origin: origin}, origin, "", s, "flag-token"},
		{"env wins over store", Credential{Origin: origin}, origin, "env-token", s, "env-token"},
		{"store fallback", Credential{Origin: origin}, origin, "", s, "stored-token"},
		{"nil store", Credential{Origin: origin}, origin, "", nil, ""},
		{"unknown host", Credential{Origin: origin}, "unknown:6309", "", s, ""},
		{"explicit wins even with env", Credential{Explicit: "flag-token", Origin: origin}, origin, "env-token", s, "flag-token"},
		{"explicit never reaches a foreign host", Credential{Explicit: "flag-token", Origin: origin}, "evil.example:6309", "", s, ""},
		{"env never reaches a foreign host", Credential{Origin: origin}, "evil.example:6309", "env-token", s, ""},
		{"foreign host still gets its stored token", Credential{Explicit: "flag-token", Origin: origin}, "other.example:6309", "env-token", s, "other-stored"},
		{"no origin scopes flag and env to no host", Credential{Explicit: "flag-token"}, origin, "env-token", s, "stored-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DEMARKUS_AUTH", tt.envVal)
			got := Resolve(tt.cred, tt.host, tt.store)
			if got != tt.want {
				t.Errorf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := LoadDefault()
	if s == nil {
		t.Fatal("LoadDefault returned nil")
	}
	if got := s.Get("nonexistent:6309"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestGet_NilStore(t *testing.T) {
	var s *Store
	if got := s.Get("localhost:6309"); got != "" {
		t.Errorf("expected empty, got %q", got)
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

func TestLoadDefaultReportsBrokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".mark"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".mark", "tokens.toml"), []byte("not [valid"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged []string
	warnf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { warnf = log.Printf })

	if s := LoadDefault(); s == nil || s.Get("any:6309") != "" {
		t.Fatalf("want an empty store, got %v", s)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "tokens.toml") {
		t.Errorf("logged = %q, want one line naming the file", logged)
	}
}

// A reader racing a save must never see a truncated, empty file.
func TestSaveNeverExposesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.toml")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("keep:6309", "tok"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		for i := range 200 {
			if err := s.Set("churn:6309", fmt.Sprint(i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		r, err := Load(path)
		if err != nil {
			t.Fatalf("load during save: %v", err)
		}
		if r.Get("keep:6309") != "tok" {
			t.Fatal("reader saw a store without the kept token")
		}
	}
}
