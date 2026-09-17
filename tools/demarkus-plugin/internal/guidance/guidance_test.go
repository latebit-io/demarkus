package guidance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dm := filepath.Join(home, ".demarkus")
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dm, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func ctx(t *testing.T, in Input) string {
	t.Helper()
	o, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return o.Context
}

func TestMemoryGuidanceHealthAndOffer(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	gfile := filepath.Join(t.TempDir(), "g.md")
	_ = os.WriteFile(gfile, []byte("# standing guidance"), 0o644)

	out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile})
	if !strings.Contains(out, "server is not running") {
		t.Error("expected health warning (no .pid for the configured memory)")
	}
	if !strings.Contains(out, "One-time setup offer") {
		t.Error("expected the one-time memory offer on first call")
	}
	if !strings.Contains(out, "standing guidance") {
		t.Error("expected the static guidance body appended")
	}
	// second call: offer is now suppressed by the sentinel
	if strings.Contains(ctx(t, Input{Surface: "memory", GuidanceFile: gfile}), "One-time setup offer") {
		t.Error("offer should show only once")
	}
}

func TestKnowledgeGuidance(t *testing.T) {
	// no systems → one-time hint, then silent
	setupHome(t, nil)
	first := ctx(t, Input{Surface: "knowledge"})
	if !strings.Contains(first, "/knowledge-join") {
		t.Error("expected the one-time join hint when nothing is joined")
	}
	if ctx(t, Input{Surface: "knowledge"}) != "" {
		t.Error("hint should show only once")
	}

	// joined → lists the system
	setupHome(t, map[string]string{"knowledge-systems": "acme\nbeta\n"})
	gfile := filepath.Join(t.TempDir(), "kg.md")
	_ = os.WriteFile(gfile, []byte("# knowledge guidance"), 0o644)
	out := ctx(t, Input{Surface: "knowledge", GuidanceFile: gfile})
	if !strings.Contains(out, "**acme**") || !strings.Contains(out, "**beta**") {
		t.Errorf("expected joined systems listed, got: %s", out[:min(120, len(out))])
	}
	if !strings.Contains(out, "No local demarkus store is configured") {
		t.Error("expected the memory↔system note when no memory is configured")
	}
}

func TestMemoryGuidanceProjectHeader(t *testing.T) {
	project := filepath.Join(t.TempDir(), "My Repo")
	setupHome(t, map[string]string{
		"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n",
		"project-souls":      project + "\tteam\n",
		"souls":              "team\tmark://team.example:6309\tfalse\t-\n",
	})
	gfile := filepath.Join(t.TempDir(), "g.md")
	if err := os.WriteFile(gfile, []byte("<!-- markdownlint-disable MD041 -->\n<!-- Code generated; DO NOT EDIT. -->\n# standing guidance"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: project})
	if !strings.Contains(out, "Project slug: `my-repo`. Bound store: `team`.") {
		t.Fatalf("header missing or wrong:\n%s", out)
	}
	if strings.Contains(out, "<!--") || !strings.Contains(out, "\n\n# standing guidance") {
		t.Fatalf("generator comments must be stripped from the injected guidance:\n%s", out)
	}
	if strings.Index(out, "Project slug") > strings.Index(out, "standing guidance") {
		t.Fatal("header must precede the static guidance")
	}
	t.Setenv("CURSOR_PROJECT_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(t.TempDir(), "other"))
	if out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile}); !strings.Contains(out, "Project slug: `other`. Bound store: `demarkus-memory` (local, no project binding).") {
		t.Fatalf("unbound project header wrong:\n%s", out)
	}
	stale := filepath.Join(t.TempDir(), "stale")
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n", "project-souls": stale + "\tgone\n"})
	if out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: stale}); !strings.Contains(out, "Bound store: `gone` (stale: not in the catalog; restore it with /soul-join or rebind with /soul-default).") {
		t.Fatalf("stale binding must be marked:\n%s", out)
	}
}

func TestGuidanceSurfacesUnreadableFileAndRelativeProjectDir(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	if _, err := Evaluate(Input{Surface: "memory", GuidanceFile: filepath.Join(t.TempDir(), "missing.md")}); err == nil {
		t.Fatal("a configured guidance file that cannot be read must be an error, not silent empty guidance")
	}
	project := filepath.Join(t.TempDir(), "rel-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	setupHome(t, map[string]string{
		"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n",
		"project-souls":      project + "\tteam\n",
		"souls":              "team\tmark://team.example:6309\tfalse\t-\n",
	})
	t.Chdir(project)
	if out := ctx(t, Input{Surface: "memory", ProjectDir: "."}); !strings.Contains(out, "Project slug: `rel-project`. Bound store: `team`.") {
		t.Fatalf("relative project dir must resolve to its binding:\n%s", out)
	}
}

func TestMemoryGuidanceSurvivesInvalidProjectName(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	out := ctx(t, Input{Surface: "memory", ProjectDir: filepath.Join(t.TempDir(), "bad#name")})
	if !strings.Contains(out, "Project slug: none (directory name \"bad#name\" is not a valid slug") {
		t.Fatalf("an unusable directory name must be stated, not drop the guidance:\n%s", out)
	}
}
