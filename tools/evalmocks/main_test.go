package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/reporoot"
)

// rendered is the repository's artifacts, rendered once for every test.
var rendered = sync.OnceValues(func() (string, []artifact) {
	root, err := reporoot.Find(filepath.Join(evalsDir, runnerFile))
	if err != nil {
		panic(err)
	}
	artifacts, err := renderAll(root)
	if err != nil {
		panic(err)
	}
	return root, artifacts
})

// Every mock renders, and the hand-written ones exist and are not also
// rendered. Whether the files are current is `evalmocks check`, in pre-commit.
func TestRepositoryMocksRender(t *testing.T) {
	root, artifacts := rendered()
	for _, rel := range handWritten {
		if _, err := os.Stat(mockPath(root, rel)); err != nil {
			t.Errorf("hand-written mock listed but absent: %v", err)
		}
		if slices.ContainsFunc(artifacts, func(a artifact) bool { return a.Rel == rel }) {
			t.Errorf("%s is both rendered and listed as hand-written", rel)
		}
	}
}

// The tool list is the runner's saved tools/list over the lean golden, text
// untouched: what the plugin's mcp-serve starts.
func TestToolListWrapsTheLeanGolden(t *testing.T) {
	root, artifacts := rendered()
	text := string(artifacts[0].Content)
	if !strings.HasPrefix(text, "{\n  \"tools\": [\n") {
		t.Fatalf("not a tools/list result:\n%.80s", text)
	}
	golden, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(toolsGold)))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(golden)), "\n")[1:] {
		if line != "]" && !strings.Contains(text, line) {
			t.Errorf("golden line not carried verbatim: %q", line)
		}
	}
}

// A runner placeholder replaces a literal the rendering must contain; a
// fixture that stops producing it is a broken template, not a silent mock.
func TestSubstRequiresTheLiteral(t *testing.T) {
	m := mocks[0]
	m.Subst = map[string]string{"/not/in/the/rendering": "{{input.url}}"}
	if _, err := m.render(); err == nil || !strings.Contains(err.Error(), "nothing to template") {
		t.Fatalf("render with an absent literal: %v", err)
	}
}

// Check names a stale file and an unlisted one, each once, and nothing else.
func TestCheckReportsDriftAndUnlistedMocks(t *testing.T) {
	_, artifacts := rendered()
	tmp := t.TempDir()
	if err := writeAll(tmp, artifacts); err != nil {
		t.Fatal(err)
	}
	for _, rel := range append(handWritten, "journal/"+mocksLeaf+"/mark_versions.md") {
		if err := os.MkdirAll(filepath.Dir(mockPath(tmp, rel)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mockPath(tmp, rel), []byte("status: ok\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(mockPath(tmp, artifacts[1].Rel), []byte("edited by hand\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkAll(tmp, artifacts)
	if err == nil {
		t.Fatal("check passed on a stale and an unlisted mock")
	}
	msg := err.Error()
	for _, want := range []string{"stale eval mocks", artifacts[1].Rel, "neither rendered nor listed", "journal/" + mocksLeaf + "/mark_versions.md"} {
		if strings.Count(msg, want) != 1 {
			t.Errorf("check error names %q %d times:\n%s", want, strings.Count(msg, want), msg)
		}
	}
	if strings.Contains(msg, handWritten[0]) {
		t.Errorf("hand-written mock reported:\n%s", msg)
	}
}
