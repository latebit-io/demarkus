package facts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		cmd  string
		want Command
	}{
		{"go test ./...", Command{[]Check{Test}, true}},
		{"cd tools && go test ./internal/... -race", Command{[]Check{Test}, true}},
		{"CGO_ENABLED=0 go build ./...", Command{[]Check{Build}, true}},
		{"bash pre-commit.sh", Command{[]Check{PreCommit}, true}},
		{"./pre-commit.sh;", Command{[]Check{PreCommit}, true}},
		{"/usr/local/go/bin/go vet ./...", Command{[]Check{Lint}, true}},
		{"make", Command{[]Check{Build}, true}},
		{"make test", Command{[]Check{Test}, true}},
		{"go test ./... && bash pre-commit.sh", Command{[]Check{Test, PreCommit}, true}},
		{"go build ./... && go test ./... 2>&1 | tail -20", Command{[]Check{Build, Test}, false}},
		{"go test ./... ; echo done", Command{[]Check{Test}, false}},
		{"go test ./... || true", Command{[]Check{Test}, false}},
		{"git status | head", Command{Attributable: true}},
		{`git commit -m "fix && go test"`, Command{Attributable: true}},
		{"echo go test", Command{Attributable: true}},
		{"", Command{Attributable: true}},
	}
	for _, c := range cases {
		if got := Classify(c.cmd); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Classify(%q) = %+v, want %+v", c.cmd, got, c.want)
		}
	}
}

func TestOutcome(t *testing.T) {
	one := Command{[]Check{Test}, true}
	two := Command{[]Check{Test, PreCommit}, true}
	piped := Command{[]Check{Test}, false}
	cases := []struct {
		cmd    Command
		exitOK bool
		want   Outcome
	}{
		{one, true, Passed},
		{one, false, Failed},
		{two, true, Passed},
		{two, false, Unknown},
		{piped, true, Unknown},
		{piped, false, Unknown},
	}
	for _, c := range cases {
		if got := c.cmd.Outcome(c.exitOK); got != c.want {
			t.Errorf("%+v exitOK=%v: got %q, want %q", c.cmd, c.exitOK, got, c.want)
		}
	}
}

func TestRender(t *testing.T) {
	cases := []struct {
		name string
		s    Session
		want string
	}{
		{"nothing", Session{}, ""},
		{"checks only", Session{Checks: map[Check]Outcome{}}, "Where it stopped:\n- checks: none recognized\n"},
		{
			"full",
			Session{
				Git:    &Git{Branch: "feat/x", Head: "35c78b30aa11", Uncommitted: []string{"a.go", "b.go"}},
				Checks: map[Check]Outcome{PreCommit: Passed, Test: Failed},
			},
			"Where it stopped:\n- branch: feat/x at 35c78b30\n- uncommitted (2): a.go, b.go\n- checks: test failed; pre-commit passed\n",
		},
		{
			"repo named",
			Session{Git: &Git{Root: "/src/demarkus", Branch: "feat/x", Head: "35c78b30aa"}},
			"Where it stopped:\n- branch: feat/x at 35c78b30 in demarkus\n- uncommitted: none\n",
		},
		{
			"unknown results grouped",
			Session{Checks: map[Check]Outcome{Test: Passed, Lint: Unknown, Build: Unknown}},
			"Where it stopped:\n- checks: test passed; lint, build ran, result not visible\n",
		},
		{
			"clean detached, no transcript",
			Session{Git: &Git{Head: "abc"}},
			"Where it stopped:\n- branch: detached HEAD at abc\n- uncommitted: none\n",
		},
		{
			"unborn",
			Session{Git: &Git{Branch: "main"}},
			"Where it stopped:\n- branch: main (no commits yet)\n- uncommitted: none\n",
		},
	}
	for _, c := range cases {
		if got := Render(c.s); got != c.want {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRenderBounds(t *testing.T) {
	var paths []string
	for i := range 30 {
		paths = append(paths, fmt.Sprintf("p%02d.go", i))
	}
	got := Render(Session{Git: &Git{Branch: "b", Head: "h", Uncommitted: paths}})
	if !strings.Contains(got, "uncommitted (30): p00.go") || !strings.Contains(got, "p19.go, +10 more") || strings.Contains(got, "p20.go") {
		t.Errorf("want 20 paths then +10 more, got %q", got)
	}

	long := strings.Repeat("x", 300)
	paths = paths[:0]
	for i := range 20 {
		paths = append(paths, fmt.Sprintf("%s%02d", long, i))
	}
	got = Render(Session{Git: &Git{Branch: "b", Head: "h", Uncommitted: paths}})
	if len(got) > maxBlock {
		t.Errorf("block is %d bytes, cap %d", len(got), maxBlock)
	}
	if !strings.Contains(got, "more") {
		t.Errorf("trimmed paths must be counted, got %q", got)
	}
}

func TestParseStatus(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 35c78b30aa11bb22",
		"# branch.head feat/x",
		"# branch.upstream origin/feat/x",
		"1 .M N... 100644 100644 100644 aaa bbb dir/mod ified.go",
		"2 R. N... 100644 100644 100644 aaa bbb R100 new.go",
		"old.go",
		"u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.go",
		"? untracked.txt",
		"",
	}, "\x00")
	got := parseStatus([]byte(out))
	want := Git{Branch: "feat/x", Head: "35c78b30aa11bb22", Uncommitted: []string{"dir/mod ified.go", "new.go", "conflict.go", "untracked.txt"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}

	got = parseStatus([]byte("# branch.oid (initial)\x00# branch.head (detached)\x00"))
	if got.Head != "" || got.Branch != "" || got.Uncommitted != nil {
		t.Errorf("initial/detached must be empty, got %+v", got)
	}
}

func TestReadGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("a.go")
	git("add", "a.go")
	git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	write("b.go")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := ReadGit(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReadGit: %v", err)
	}
	if g.Branch != "main" || len(g.Head) < 7 || !reflect.DeepEqual(g.Uncommitted, []string{"a.go", "b.go"}) {
		t.Errorf("got %+v", g)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Root != want {
		t.Errorf("Root = %q, want %q", g.Root, want)
	}

	if _, err := ReadGit(context.Background(), t.TempDir()); err == nil {
		t.Error("a directory outside any repo must error")
	}
}
