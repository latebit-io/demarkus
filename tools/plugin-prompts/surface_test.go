package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckToolSurface(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "plugins", "prompt-source", "memory", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ok.md.tmpl", "Use mark_lookup then mark_fetch; mark_worlds lists worlds.\n")
	if err := checkToolSurface(root); err != nil {
		t.Fatalf("lean references rejected: %v", err)
	}
	write("bad.md.tmpl", "Run mark_graph.\nThen mark_graph_export the store.\n")
	err := checkToolSurface(root)
	if err == nil || !strings.Contains(err.Error(), "bad.md.tmpl:2 references mark_graph_export") {
		t.Fatalf("advanced reference not reported: %v", err)
	}
}

func TestRepositoryPromptsStayOnLeanSurface(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Skip("not inside the repository")
	}
	if err := checkToolSurface(root); err != nil {
		t.Fatal(err)
	}
}
