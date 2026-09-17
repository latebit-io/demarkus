package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if r.Header() != "Project slug: `my-repo`. Bound store: `team`." {
		t.Fatalf("header: %s", r.Header())
	}
	if r.Lines() != "slug=my-repo\nstore=team\nstate=bound" {
		t.Fatalf("lines: %s", r.Lines())
	}

	other := filepath.Join(t.TempDir(), "other")
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	r, err = Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != other || r.Store != "demarkus-memory" || r.State != StateLocal {
		t.Fatalf("harness fallback wrong: %+v", r)
	}
	if r.Header() != "Project slug: `other`. Bound store: `demarkus-memory` (local, no project binding)." {
		t.Fatalf("local header: %s", r.Header())
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
	if r.Header() != "Project slug: `stale`. Bound store: `gone` (stale: not in the catalog; restore it with /soul-join or rebind with /soul-default)." {
		t.Fatalf("stale header: %s", r.Header())
	}
	if r.Lines() != "slug=stale\nstore=gone\nstate=stale\nhint=not in the catalog; restore it with /soul-join or rebind with /soul-default" {
		t.Fatalf("stale lines: %s", r.Lines())
	}
	local := filepath.Join(t.TempDir(), "local")
	home(t, map[string]string{"project-souls": local + "\tdemarkus-memory\n"})
	r, err = Resolve(local)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateStale || !strings.Contains(r.Lines(), "hint=not in the catalog; restore it with /soul-init") {
		t.Fatalf("a stale local binding must point at /soul-init: %+v %s", r, r.Lines())
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
