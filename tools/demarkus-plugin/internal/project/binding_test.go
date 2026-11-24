package project

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project/projecttest"
)

const catalogRows = "team\tmark://team.example:6309\tfalse\t-\npersonal\tmark://me.example:6309\tfalse\t-\n"

// bindings stages ~/.demarkus with the catalog and the given rows.
func bindings(t *testing.T, rows ...[2]string) {
	t.Helper()
	body := ""
	for _, r := range rows {
		body += r[0] + "\t" + r[1] + "\n"
	}
	home(t, map[string]string{"plugin-memory.conf": memoryConf, "souls": catalogRows, "project-souls": body})
}

func boundStore(t *testing.T, dir string) (string, bool) {
	t.Helper()
	store, inherited, err := BoundStore(dir)
	if err != nil {
		t.Fatalf("BoundStore(%s): %v", dir, err)
	}
	return store, inherited
}

// layout is one repository with a sibling worktree, in separate parents.
func layout(t *testing.T) (main, wt string) {
	t.Helper()
	root := t.TempDir()
	main = filepath.Join(root, "home", "src", "app")
	wt = filepath.Join(root, "work", "client", "app.feat")
	projecttest.Repo(t, main, projecttest.Origin("https://example.com/org/app.git"))
	projecttest.Worktree(t, filepath.Join(main, ".git"), wt)
	if err := os.MkdirAll(filepath.Join(wt, "tools"), 0o750); err != nil {
		t.Fatal(err)
	}
	return main, wt
}

// TestWorktreeWritesToTheBoundStore: a worktree of a bound project is bound.
func TestWorktreeWritesToTheBoundStore(t *testing.T) {
	main, wt := layout(t)
	bindings(t, [2]string{main, "team"})

	if store, inherited := boundStore(t, main); store != "team" || inherited {
		t.Fatalf("main checkout: %q inherited=%v", store, inherited)
	}
	for _, dir := range []string{wt, filepath.Join(wt, "tools")} {
		if store, inherited := boundStore(t, dir); store != "team" || !inherited {
			t.Fatalf("%s: %q inherited=%v, want the main checkout's team", dir, store, inherited)
		}
	}
	r, err := Resolve(wt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir != wt || r.Slug != "app" || r.Store != "team" || r.State != StateBound || !r.Inherited || !r.Linked {
		t.Fatalf("worktree resolution: %+v", r)
	}
}

func TestBindingPrecedence(t *testing.T) {
	main, wt := layout(t)
	mainParent, wtParent := filepath.Dir(main), filepath.Dir(wt)
	cases := []struct {
		name      string
		rows      [][2]string
		store     string
		inherited bool
	}{
		{"unbound", nil, "", false},
		{"the worktree's own row beats the main checkout's",
			[][2]string{{main, "team"}, {wt, "personal"}}, "personal", false},
		{"the main checkout's row beats what encloses the worktree",
			[][2]string{{wtParent, "personal"}, {main, "team"}}, "team", true},
		// Where the worktree sits is the user's own choice of store; the
		// main checkout's surroundings say nothing about this directory.
		{"what encloses the worktree beats what encloses the main checkout",
			[][2]string{{mainParent, "personal"}, {wtParent, "team"}}, "team", false},
		{"what encloses the main checkout is the last resort",
			[][2]string{{mainParent, "personal"}}, "personal", true},
		{"a row for another directory is not a match",
			[][2]string{{main + "-other", "team"}}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bindings(t, c.rows...)
			if store, inherited := boundStore(t, wt); store != c.store || inherited != c.inherited {
				t.Fatalf("BoundStore = %q inherited=%v, want %q inherited=%v", store, inherited, c.store, c.inherited)
			}
		})
	}

	// A subdirectory looks at the same path in the main checkout first.
	bindings(t, [2]string{main, "personal"}, [2]string{filepath.Join(main, "tools"), "team"})
	if store, inherited := boundStore(t, filepath.Join(wt, "tools")); store != "team" || !inherited {
		t.Fatalf("mirrored subdirectory: %q inherited=%v", store, inherited)
	}
	if store, _ := boundStore(t, wt); store != "personal" {
		t.Fatalf("the worktree top must not take a subdirectory's row: %q", store)
	}
}

// TestBindingOutsideAWorktreeIsUnchanged: a main checkout and a plain
// directory keep the nearest-ancestor walk they always had.
func TestBindingOutsideAWorktreeIsUnchanged(t *testing.T) {
	main, _ := layout(t)
	plain := filepath.Join(t.TempDir(), "notes", "deep")
	bindings(t, [2]string{filepath.Dir(main), "personal"}, [2]string{filepath.Dir(plain), "team"})
	if store, inherited := boundStore(t, filepath.Join(main, "pkg")); store != "personal" || inherited {
		t.Fatalf("main checkout subdirectory: %q inherited=%v", store, inherited)
	}
	if store, inherited := boundStore(t, plain); store != "team" || inherited {
		t.Fatalf("plain directory: %q inherited=%v", store, inherited)
	}
	if store, _, err := BoundStore(""); err != nil || store != "" {
		t.Fatalf("no directory: %q, %v", store, err)
	}
}

// TestBindingMatchesThroughSymlinks: git records the main checkout's real
// path, a harness the path the user typed.
func TestBindingMatchesThroughSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "volumes", "code", "app")
	projecttest.Repo(t, target, projecttest.Origin("https://example.com/org/app.git"))
	wt := filepath.Join(root, "wt", "app.feat")
	projecttest.Worktree(t, filepath.Join(target, ".git"), wt)
	link := filepath.Join(root, "code")
	if err := os.Symlink(filepath.Join(root, "volumes", "code"), link); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(link, "app")

	bindings(t, [2]string{filepath.Join(root, "gone", "repo"), "personal"}, [2]string{typed, "team"})
	if store, inherited := boundStore(t, wt); store != "team" || !inherited {
		t.Fatalf("row spelled through a symlink: %q inherited=%v", store, inherited)
	}
	bindings(t, [2]string{link, "team"})
	if store, inherited := boundStore(t, wt); store != "team" || !inherited {
		t.Fatalf("ancestor row spelled through a symlink: %q inherited=%v", store, inherited)
	}
}

func TestBindingAndBrokenCheckouts(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "broken")
	projecttest.Broken(t, broken, "not a pointer\n")

	// Nothing to hold a write to: the checkout is never read.
	bindings(t)
	if store, _, err := BoundStore(broken); err != nil || store != "" {
		t.Fatalf("no rows: %q, %v", store, err)
	}
	// With rows, a guess could send the write to the wrong store.
	bindings(t, [2]string{filepath.Dir(broken), "team"})
	if _, _, err := BoundStore(broken); !errors.As(err, new(*IdentityError)) {
		t.Fatalf("broken checkout with rows: err = %v, want an IdentityError", err)
	}
	if _, err := BindTarget(broken); !errors.As(err, new(*IdentityError)) {
		t.Fatalf("BindTarget on a broken checkout: err = %v, want an IdentityError", err)
	}
}

func TestBindTarget(t *testing.T) {
	main, wt := layout(t)
	sub := filepath.Join(wt, "tools")
	plain := filepath.Join(t.TempDir(), "plain")

	// Nothing recorded: nothing to replace, and the target is the main checkout's path.
	bindings(t)
	for dir, want := range map[string]string{main: main, filepath.Join(main, "tools"): filepath.Join(main, "tools"), plain: plain, wt: main, sub: filepath.Join(main, "tools")} {
		if got, err := BindTarget(dir); err != nil || got.Dir != want || got.Replaces != nil {
			t.Fatalf("BindTarget(%s) = %+v, %v; want %s replacing nothing", dir, got, err, want)
		}
	}
	if _, err := BindTarget(""); !errors.Is(err, ErrNoDir) {
		t.Fatalf("BindTarget(\"\") = %v, want ErrNoDir", err)
	}

	// The worktree's own rows would outrank the main checkout's: they go.
	bindings(t, [2]string{wt, "team"}, [2]string{sub, "team"}, [2]string{plain, "team"})
	if got, err := BindTarget(wt); err != nil || got.Dir != main || !slices.Equal(got.Replaces, []string{wt}) {
		t.Fatalf("BindTarget(worktree) = %+v, %v", got, err)
	}
	if got, err := BindTarget(sub); err != nil || !slices.Equal(got.Replaces, []string{sub, wt}) {
		t.Fatalf("BindTarget(worktree subdirectory) = %+v, %v", got, err)
	}

	// One row per directory: other spellings of the target and of the
	// worktree go too, or a lookup through one would find the old store.
	links := t.TempDir()
	for name, to := range map[string]string{"main": filepath.Dir(main), "wt": filepath.Dir(wt)} {
		if err := os.Symlink(to, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
	}
	typedMain, typedWT := filepath.Join(links, "main", "app"), filepath.Join(links, "wt", "app.feat")
	bindings(t, [2]string{typedMain, "team"}, [2]string{typedWT, "team"})
	if got, err := BindTarget(wt); err != nil || got.Dir != main || !slices.Equal(got.Replaces, []string{typedMain, typedWT}) {
		t.Fatalf("BindTarget with rows under other spellings = %+v, %v", got, err)
	}
}

// TestBindReachesEveryWorktree: a default set anywhere in a repository is
// the repository's. A row another worktree still holds would keep that
// worktree on the old store.
func TestBindReachesEveryWorktree(t *testing.T) {
	main, wt := layout(t)
	other := filepath.Join(filepath.Dir(wt), "app.other")
	projecttest.Worktree(t, filepath.Join(main, ".git"), other)

	// A record naming a directory that is no worktree of this repository
	// must not cost that directory its row.
	victim := filepath.Join(t.TempDir(), "victim")
	projecttest.Repo(t, victim, "")
	projecttest.Write(t, filepath.Join(main, ".git", "worktrees", "forged", "gitdir"), filepath.Join(victim, ".git")+"\n")

	bindings(t, [2]string{other, "team"}, [2]string{victim, "personal"}, [2]string{main, "team"})
	for _, from := range []string{wt, main} {
		got, err := BindTarget(from)
		if err != nil || got.Dir != main || !slices.Equal(got.Replaces, []string{other}) {
			t.Fatalf("BindTarget(%s) = %+v, %v; want %s replacing only %s", from, got, err, main, other)
		}
	}
}

// TestBindRefusesAnIncompleteWorktreeList: a bind that could not read the
// repository's worktrees must write nothing. A list cut short would leave a
// worktree on its old row while the command reported success.
func TestBindRefusesAnIncompleteWorktreeList(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	main, wt := layout(t)
	bindings(t, [2]string{wt, "team"})
	records := filepath.Join(main, ".git", "worktrees")
	for name, path := range map[string]string{"the records directory": records, "one record": filepath.Join(records, filepath.Base(wt), "gitdir")} {
		if err := os.Chmod(path, 0o000); err != nil {
			t.Fatal(err)
		}
		for _, from := range []string{main, wt} {
			if got, err := BindTarget(from); err == nil {
				t.Fatalf("%s unreadable: BindTarget(%s) = %+v, want an error", name, from, got)
			}
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
	}
	// A record whose worktree is gone is ordinary: pruned, not unreadable.
	if err := os.Remove(filepath.Join(records, filepath.Base(wt), "gitdir")); err != nil {
		t.Fatal(err)
	}
	if got, err := BindTarget(main); err != nil || got.Dir != main {
		t.Fatalf("pruned record: BindTarget = %+v, %v", got, err)
	}
}

// TestOneBindingUnderEverySpelling: a bind from a worktree records git's
// spelling of the main checkout; a session that reached the main checkout
// through a symlink must still find it, or the repository splits again.
func TestOneBindingUnderEverySpelling(t *testing.T) {
	main, wt := layout(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(main), link); err != nil {
		t.Fatal(err)
	}
	bindings(t, [2]string{main, "team"})
	for _, dir := range []string{filepath.Join(link, "app"), filepath.Join(link, "app", "pkg"), wt} {
		if store, _ := boundStore(t, dir); store != "team" {
			t.Fatalf("%s is bound to %q, want team", dir, store)
		}
	}
}
