package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the CLI into a temp dir, mirroring buildAndVersion.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "demarkus-token")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// TestRevokeIfPresent pins -if-present semantics: absent label or absent file
// succeed (the fresh-mint case), while real store failures still error.
func TestRevokeIfPresent(t *testing.T) {
	bin := buildBinary(t)
	tokens := filepath.Join(t.TempDir(), "tokens.toml")
	run := func(args ...string) error {
		return exec.Command(bin, args...).Run()
	}
	read := func() string {
		t.Helper()
		b, err := os.ReadFile(tokens)
		if err != nil {
			t.Fatalf("read tokens.toml: %v", err)
		}
		return string(b)
	}

	if err := run("revoke", "-if-present", "-label", "x", "-tokens", tokens); err != nil {
		t.Errorf("-if-present on absent file = %v, want nil", err)
	}
	if err := run("revoke", "-label", "x", "-tokens", tokens); err == nil {
		t.Error("plain revoke on absent file succeeded")
	}

	if err := run("generate", "-label", "keep", "-tokens", tokens); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := run("revoke", "-if-present", "-label", "missing", "-tokens", tokens); err != nil {
		t.Errorf("-if-present on missing label = %v, want nil", err)
	}
	if !strings.Contains(read(), "tokens.keep]") {
		t.Error("-if-present on a missing label removed an unrelated entry")
	}
	if err := run("revoke", "-if-present", "-label", "keep", "-tokens", tokens); err != nil {
		t.Errorf("-if-present on present label = %v, want nil", err)
	}
	if strings.Contains(read(), "tokens.keep]") {
		t.Error("present label was not revoked")
	}

	// Real store failures are not absorbed by -if-present.
	if err := os.WriteFile(tokens, []byte("not toml [[["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run("revoke", "-if-present", "-label", "x", "-tokens", tokens); err == nil {
		t.Error("-if-present on a malformed store succeeded")
	}
}
