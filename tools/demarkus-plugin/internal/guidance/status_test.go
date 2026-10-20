package guidance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/facts"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
)

func stubProbe(t *testing.T, st ProjectStatus) {
	t.Helper()
	prev := probeProject
	probeProject = func(context.Context, string, string) ProjectStatus { return st }
	t.Cleanup(func() { probeProject = prev })
}

var app = project.Resolution{Slug: "app", Store: "soul", State: project.StateBound}

func TestStatusRendering(t *testing.T) {
	git := &facts.Git{Branch: "feat/x", Head: "0123456789", Uncommitted: []string{"a.go", "b.go", "c.go"}}
	tests := []struct {
		name               string
		s                  sessionStatus
		warn               string
		header, user, deny string
	}{
		{name: "known",
			s:      sessionStatus{project: ProjectStatus{State: ProjectKnown, Root: "/app/", LastJournal: "2026-10-13", JournalPath: "/app/journal/2026-10-13.md"}, git: git},
			header: "Project slug: `app`. Project root: `/app/`. Bound store: `soul`. Project: known, newest journal entry `/app/journal/2026-10-13.md`. Verdict: use\n\nWhere it stands:\n- branch: feat/x at 01234567",
			user:   "demarkus: soul soul · project app · last journal 2026-10-13 · feat/x, 3 uncommitted"},
		{name: "root project, no journal",
			s:      sessionStatus{project: ProjectStatus{State: ProjectKnown, Root: "/", NoJournal: true}, gitNote: "not a git repository"},
			header: "Project root: `/`. Bound store: `soul`. Project: known, no journal entry yet. Verdict: use\n\nWhere it stands: no git facts (not a git repository).",
			user:   "demarkus: soul soul · project app"},
		{name: "new",
			s:      sessionStatus{project: ProjectStatus{State: ProjectNew, Root: "/app/"}, git: git},
			header: "Project: new, no hub at `/app/index.md` yet.",
			user:   "demarkus: soul soul · project app (new) · feat/x, 3 uncommitted"},
		{name: "unknown never renders as new",
			s:      sessionStatus{project: ProjectStatus{State: ProjectUnknown, Root: "/app/", Reason: "no answer within 3s"}, git: git},
			header: "Project: unknown (no answer within 3s). Treat recall misses as inconclusive. Verdict: use, root unconfirmed: no answer within 3s",
			user:   "demarkus: soul soul · project app (status unknown: no answer within 3s) · feat/x, 3 uncommitted",
			deny:   "new"},
		{name: "health warning replaces the tail",
			s:    sessionStatus{project: ProjectStatus{State: ProjectKnown, Root: "/app/", LastJournal: "2026-10-13"}, git: git},
			warn: "server is not running",
			user: "demarkus: soul soul · project app · ⚠️ server is not running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header, user := tt.s.agentHeader(app), tt.s.userLine(app, tt.warn, baseBrand["memory"])
			if !strings.Contains(header, tt.header) {
				t.Errorf("header = %q, want %q", header, tt.header)
			}
			if user != tt.user {
				t.Errorf("user = %q, want %q", user, tt.user)
			}
			if tt.deny != "" && (strings.Contains(header, tt.deny) || strings.Contains(user, tt.deny)) {
				t.Errorf("%q leaked into %q / %q", tt.deny, header, user)
			}
		})
	}
}

func TestReadStatusGitFacts(t *testing.T) {
	stubProbe(t, ProjectStatus{State: ProjectKnown, Root: "/app/"})
	repo := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q", "-b", "feat/x").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := app
	r.Dir = repo
	s := readStatus(r)
	if h := s.agentHeader(r); !strings.Contains(h, "Where it stands:\n- branch: feat/x (no commits yet) in app\n- uncommitted (1): a.go") || strings.Contains(h, "checks") {
		t.Errorf("repo header = %q", h)
	}
	r.Dir = t.TempDir()
	s = readStatus(r)
	if h := s.agentHeader(r); !strings.HasSuffix(h, "Where it stands: no git facts (not a git repository).") {
		t.Errorf("non-repo header = %q", h)
	}
}

// A store that never answers and a git that hangs both stay inside the budget.
func TestReadStatusKeepsTheDeadline(t *testing.T) {
	probeProject = func(ctx context.Context, _, slug string) ProjectStatus {
		<-ctx.Done()
		return unknownStatus(slug, failure(ctx, ctx.Err()))
	}
	t.Cleanup(func() { probeProject = probeStore })
	bin := t.TempDir()
	// No exec: the sleep child keeps git's pipes open after the kill.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := app
	r.Dir = t.TempDir()
	start := time.Now()
	s := readStatus(r)
	if elapsed := time.Since(start); elapsed > statusBudget+time.Second {
		t.Errorf("session status took %s, budget %s", elapsed, statusBudget)
	}
	if s.project.State != ProjectUnknown || s.git != nil || !strings.Contains(s.gitNote, "did not answer") {
		t.Errorf("status = %+v", s)
	}
}

// A stale binding's header carries the stop verdict the store partial acts on.
func TestAgentHeaderCarriesTheStopVerdict(t *testing.T) {
	stale := project.Resolution{Slug: "app", Store: "gone", State: project.StateStale}
	s := sessionStatus{project: ProjectStatus{State: ProjectKnown, Root: "/app/"}, gitNote: "not a git repository"}
	h := s.agentHeader(stale)
	for _, want := range []string{" Verdict: stop:", "/soul-join", "/soul-default"} {
		if !strings.Contains(h, want) {
			t.Errorf("header = %q, want %q", h, want)
		}
	}
}
