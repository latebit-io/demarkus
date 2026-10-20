package guidance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupHome also stubs the store probe: no test dials out.
func setupHome(t *testing.T, files map[string]string) string {
	t.Helper()
	stubProbe(t, ProjectStatus{State: ProjectUnknown, Root: "/stub/", Reason: "not probed"})
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

func eval(t *testing.T, in Input) Output {
	t.Helper()
	o, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return o
}

func ctx(t *testing.T, in Input) string { return eval(t, in).Context }

// stubHealth stands in the server health check.
func stubHealth(t *testing.T, warn string) {
	t.Helper()
	prev := serverHealthWarning
	serverHealthWarning = func() (string, error) { return warn, nil }
	t.Cleanup(func() { serverHealthWarning = prev })
}

func TestMemoryGuidanceHealth(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	gfile := filepath.Join(t.TempDir(), "g.md")
	_ = os.WriteFile(gfile, []byte("# standing guidance"), 0o644)

	out := eval(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: t.TempDir()})
	if !strings.Contains(out.Context, "server is not running") {
		t.Error("expected health warning (no .pid for the configured memory)")
	}
	if !strings.Contains(out.Context, "standing guidance") {
		t.Error("expected the static guidance body appended")
	}
	// A warning owns the status line; the offer waits for a healthy session.
	if strings.Contains(out.Status, memoryOfferText) {
		t.Errorf("offer beside a warning: %q", out.Status)
	}
}

// The one-time offer is the user's to act on: on the status line, never in
// the context, once, and not spent on a host that shows no status line.
func TestMemoryOfferOnTheStatusLineOnce(t *testing.T) {
	project := filepath.Join(t.TempDir(), "app")
	setupHome(t, map[string]string{"project-souls": project + "\tteam\n", "souls": "team\tmark://team.example:6309\tfalse\t-\n"})
	stubHealth(t, "")
	in := Input{Surface: "memory", ProjectDir: project}

	if out := eval(t, Input{Surface: "memory", ProjectDir: project, NoStatus: true}); strings.Contains(out.Status, memoryOfferText) {
		t.Errorf("offer on a host without a status line: %q", out.Status)
	}
	first := eval(t, in)
	if !strings.HasSuffix(first.Status, " · "+memoryOfferText) || strings.Contains(first.Context, memoryOfferText) {
		t.Errorf("offer: status %q, context %q", first.Status, first.Context)
	}
	if strings.Contains(eval(t, in).Status, memoryOfferText) {
		t.Error("offer should show only once")
	}
}

// stubBroker stands in the broker probe: acme answers, beta refuses.
func stubBroker(t *testing.T) {
	t.Helper()
	prev := probeBroker
	probeBroker = func(_ context.Context, u string) error {
		if strings.Contains(u, "beta") {
			return errors.New("broker unreachable at " + u)
		}
		return nil
	}
	t.Cleanup(func() { probeBroker = prev })
}

func TestKnowledgeGuidance(t *testing.T) {
	// no systems → one-time hint in context, the status line every session
	setupHome(t, nil)
	first := eval(t, Input{Surface: "knowledge"})
	if !strings.Contains(first.Context, "/knowledge-join") {
		t.Error("expected the one-time join hint when nothing is joined")
	}
	second := eval(t, Input{Surface: "knowledge"})
	if second.Context != "" {
		t.Error("hint should show only once")
	}
	if want := "demarkus-knowledge: no system joined · /knowledge-join <broker-url>"; first.Status != want || second.Status != want {
		t.Errorf("status = %q, %q", first.Status, second.Status)
	}

	// joined → each system with its probe; gamma's join recorded no url
	stubBroker(t)
	setupHome(t, map[string]string{
		"knowledge-systems": "acme\nbeta\ngamma\n",
		"knowledge-urls":    "acme\thttps://acme.example\nbeta\thttps://beta.example\n",
	})
	gfile := filepath.Join(t.TempDir(), "kg.md")
	_ = os.WriteFile(gfile, []byte("# knowledge guidance"), 0o644)
	out := eval(t, Input{Surface: "knowledge", GuidanceFile: gfile})
	for _, want := range []string{
		"- **acme** (MCP server `acme`, https://acme.example): broker reachable",
		"- **beta** (MCP server `beta`, https://beta.example): ⚠️ broker unreachable at https://beta.example",
		"- **gamma** (MCP server `gamma`; broker URL not recorded",
		knowledgeNotConnected,
		"# knowledge guidance",
		"No local demarkus store is configured",
	} {
		if !strings.Contains(out.Context, want) {
			t.Errorf("context lacks %q:\n%s", want, out.Context)
		}
	}
	if want := "demarkus-knowledge: acme reachable · beta ⚠️ broker unreachable · gamma joined"; out.Status != want {
		t.Errorf("status = %q, want %q", out.Status, want)
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
	if !strings.Contains(out, "Project slug: `my-repo`. Project root: `/stub/`. Bound store: `team`. Project: unknown (not probed).") {
		t.Fatalf("header missing or wrong:\n%s", out)
	}
	if o := eval(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: project}); !strings.HasPrefix(o.Status, "demarkus: soul team · project my-repo (status unknown: not probed)") {
		t.Fatalf("status = %q", o.Status)
	}
	if strings.Contains(out, "<!--") || !strings.Contains(out, "\n\n# standing guidance") {
		t.Fatalf("generator comments must be stripped from the injected guidance:\n%s", out)
	}
	if strings.Index(out, "Project slug") > strings.Index(out, "standing guidance") {
		t.Fatal("header must precede the static guidance")
	}
	t.Setenv("CURSOR_PROJECT_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Join(t.TempDir(), "other"))
	if out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile}); !strings.Contains(out, "Project slug: `other`. Project root: `/stub/`. Bound store: `demarkus-memory` (local, no project binding).") {
		t.Fatalf("unbound project header wrong:\n%s", out)
	}
	stale := filepath.Join(t.TempDir(), "stale")
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n", "project-souls": stale + "\tgone\n"})
	if out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: stale}); !strings.Contains(out, "Bound store: `gone` (stale: not in the catalog; restore it with /soul-join or rebind with /soul-default).") {
		t.Fatalf("stale binding must be marked:\n%s", out)
	}
}

// Without a local config an unbound project gets no header: the store it
// would name does not exist, and a sandbox must not be told to set one up. A
// project bound to a remote store keeps its header.
func TestMemoryGuidanceHeaderNeedsAStore(t *testing.T) {
	gfile := filepath.Join(t.TempDir(), "g.md")
	if err := os.WriteFile(gfile, []byte("# standing guidance\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unbound := filepath.Join(t.TempDir(), "app")
	setupHome(t, nil)
	stubHealth(t, "")
	out := eval(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: unbound})
	if out.Context != "# standing guidance\n" || out.Status != "" {
		t.Fatalf("unconfigured local store: context %q, status %q", out.Context, out.Status)
	}
	setupHome(t, map[string]string{"project-souls": unbound + "\tteam\n", "souls": "team\tmark://team.example:6309\tfalse\t-\n"})
	if out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile, ProjectDir: unbound}); !strings.Contains(out, "Bound store: `team`.") {
		t.Fatalf("a remote binding needs no local config:\n%s", out)
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
	if out := ctx(t, Input{Surface: "memory", ProjectDir: "."}); !strings.Contains(out, "Project slug: `rel-project`. Project root: `/stub/`. Bound store: `team`.") {
		t.Fatalf("relative project dir must resolve to its binding:\n%s", out)
	}
}

func TestMemoryGuidanceSurvivesInvalidProjectName(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	out := ctx(t, Input{Surface: "memory", ProjectDir: filepath.Join(t.TempDir(), "bad#name")})
	if !strings.Contains(out, "Project slug: none (directory name \"bad#name\" is not a valid slug") {
		t.Fatalf("an unusable directory name must be stated, not drop the guidance:\n%s", out)
	}
	if o := eval(t, Input{Surface: "memory", ProjectDir: filepath.Join(t.TempDir(), "bad#name")}); o.Status != `demarkus: no project (directory name "bad#name" is not a valid slug)` {
		t.Fatalf("status = %q", o.Status)
	}
}
