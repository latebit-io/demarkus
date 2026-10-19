package release

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/provisiontest"
)

// Every state a binary can be in reads as itself: a status report that said
// "missing" for a broken binary would send the user down the wrong fix.
func TestBinaryLinesKeepTheStatesDistinct(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	provisiontest.WriteBinary(t, home, "demarkus-mcp", provisiontest.VersionScript(ClientVersion))
	provisiontest.WriteBinary(t, home, "demarkus-token", provisiontest.VersionScript("0.0.1"))
	want := []string{
		"binary demarkus-server missing",
		"binary demarkus-mcp ok " + ClientVersion,
		"binary demarkus-token old 0.0.1 (pin " + ToolsRef() + ")",
	}
	if got := BinaryLines(); !slices.Equal(got, want) {
		t.Fatalf("BinaryLines = %q, want %q", got, want)
	}
	if got := installedVersions(); got != "server=,client="+ClientVersion+",tools=0.0.1" {
		t.Errorf("installedVersions = %q", got)
	}

	if err := os.WriteFile(filepath.Join(home, ".demarkus", "bin", "demarkus-server"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readBinary("demarkus-server", ServerVersion).line(); got != "binary demarkus-server not-executable" {
		t.Errorf("a file without the exec bit = %q", got)
	}
	provisiontest.WriteBinary(t, home, "demarkus-server", []byte("#!/bin/sh\nexit 3\n"))
	if got := readBinary("demarkus-server", ServerVersion).line(); got != "binary demarkus-server broken --version failed or timed out" {
		t.Errorf("a failing --version = %q", got)
	}
}
