package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSubcommandsRejectMissingAndPositionalArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"restore-empty", []string{"corpus-restore"}, "requires -root, -archive and -manifest"},
		{"restore-missing-root", []string{"corpus-restore", "-manifest", "m", "-archive", "a"}, "requires -root"},
		{"restore-positional", []string{"corpus-restore", "extra"}, "takes flags only"},
		{"pack-positional", []string{"corpus-pack", "extra"}, "takes flags only"},
		{"mcp-empty", []string{"mcp"}, "requires -mcp-bin and -host"},
		{"mcp-missing-host", []string{"mcp", "-mcp-bin", "missing"}, "requires -mcp-bin and -host"},
		{"mcp-missing-bin", []string{"mcp", "-host", "fixture"}, "requires -mcp-bin and -host"},
		{"mcp-positional", []string{"mcp", "extra"}, "takes flags only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := os.Args
			os.Args = append([]string{"answer-bench"}, tc.args...)
			t.Cleanup(func() { os.Args = original })
			if err := run(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestGraphBenchmarkRejectsUntrackedInputsBeforeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra.go"), []byte("package example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/graph-benchmark.sh")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "results")
	cmd := exec.Command("bash", script, output)
	cmd.Dir = root
	text, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(text), "untracked files") {
		t.Fatalf("unexpected result: %s: %v", text, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output created before preflight: %v", err)
	}
}
