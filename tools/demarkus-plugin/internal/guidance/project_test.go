package guidance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

func TestResolveProjectFallsBackToTheHarnessDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".demarkus"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CURSOR_PROJECT_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	if _, err := ResolveProject(""); !errors.Is(err, project.ErrNoDir) || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("no directory = %v, want ErrNoDir naming --dir", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	t.Setenv("CLAUDE_PROJECT_DIR", other)
	r, err := ResolveProject("")
	if err != nil || r.Dir != other || r.Slug != "other" || r.State != project.StateLocal {
		t.Fatalf("harness fallback = %+v, %v", r, err)
	}
	// An explicit directory wins over the harness one.
	explicit := filepath.Join(t.TempDir(), "explicit")
	if r, err := ResolveProject(explicit); err != nil || r.Dir != explicit {
		t.Fatalf("explicit dir = %+v, %v", r, err)
	}
}

func TestHeaderAndHint(t *testing.T) {
	cases := []struct {
		r    project.Resolution
		want string
	}{
		{project.Resolution{Slug: "my-repo", Store: "team", State: project.StateBound},
			"Project slug: `my-repo`. Project root: `/r/`. Bound store: `team`."},
		{project.Resolution{Slug: "other", Store: "demarkus-memory", State: project.StateLocal},
			"Project slug: `other`. Project root: `/r/`. Bound store: `demarkus-memory` (local, no project binding)."},
		{project.Resolution{Slug: "stale", Store: "gone", State: project.StateStale},
			"Project slug: `stale`. Project root: `/r/`. Bound store: `gone` (stale: not in the catalog; restore it with /soul-join or rebind with /soul-default)."},
	}
	for _, c := range cases {
		if got := Header(c.r, "/r/"); got != c.want {
			t.Errorf("Header = %s\nwant %s", got, c.want)
		}
	}
	if h := Hint(project.Resolution{Store: "demarkus-memory", State: project.StateStale}); !strings.Contains(h, "restore it with /soul-init") {
		t.Errorf("a stale local binding must point at /soul-init: %s", h)
	}
}
