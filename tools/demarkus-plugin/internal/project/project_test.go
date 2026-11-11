package project

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func home(t *testing.T, files map[string]string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	dir := filepath.Join(h, ".demarkus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CURSOR_PROJECT_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
}

const memoryConf = "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"

func TestResolveStates(t *testing.T) {
	project := filepath.Join(t.TempDir(), "My Repo")
	home(t, map[string]string{
		"plugin-memory.conf": memoryConf,
		"project-souls":      project + "\tteam\n",
		"souls":              "team\tmark://team.example:6309\tfalse\t-\n",
	})
	r, err := Resolve(project)
	if err != nil {
		t.Fatal(err)
	}
	if r.Slug != "my-repo" || r.Store != "team" || r.State != StateBound {
		t.Fatalf("bound resolution wrong: %+v", r)
	}

	// The harness variable is the surface's fallback, never the resolver's.
	other := filepath.Join(t.TempDir(), "other")
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	if _, err := Resolve(""); !errors.Is(err, ErrNoDir) {
		t.Fatalf("Resolve(\"\") with the harness variable set = %v, want ErrNoDir", err)
	}
	r, err = Resolve(other)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != other || r.Store != "demarkus-memory" || r.State != StateLocal {
		t.Fatalf("unbound resolution wrong: %+v", r)
	}

	stale := filepath.Join(t.TempDir(), "stale")
	home(t, map[string]string{"plugin-memory.conf": memoryConf, "project-souls": stale + "\tgone\n"})
	r, err = Resolve(stale)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateStale || r.Store != "gone" {
		t.Fatalf("stale resolution wrong: %+v", r)
	}
	local := filepath.Join(t.TempDir(), "local")
	home(t, map[string]string{"project-souls": local + "\tdemarkus-memory\n"})
	r, err = Resolve(local)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateStale || r.Store != "demarkus-memory" {
		t.Fatalf("a local binding without a local store is stale: %+v", r)
	}
}

func TestResolveErrors(t *testing.T) {
	home(t, map[string]string{"plugin-memory.conf": memoryConf})
	if _, err := Resolve(""); !errors.Is(err, ErrNoDir) {
		t.Fatalf("expected ErrNoDir, got %v", err)
	}
	if _, err := Resolve(filepath.Join(t.TempDir(), "bad#name")); err == nil {
		t.Fatal("a basename that is not a path segment must not become a slug")
	}
	if _, err := Slug("/"); err == nil {
		t.Fatal("root has no slug")
	}
}

func TestResolveRelativeDir(t *testing.T) {
	project := filepath.Join(t.TempDir(), "rel-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	home(t, map[string]string{
		"plugin-memory.conf": memoryConf,
		"project-souls":      project + "\tteam\n",
		"souls":              "team\tmark://team.example:6309\tfalse\t-\n",
	})
	t.Chdir(project)
	r, err := Resolve(".")
	if err != nil {
		t.Fatal(err)
	}
	if r.Store != "team" || r.Slug != "rel-project" {
		t.Fatalf("relative dir must resolve to its binding: %+v", r)
	}
}
