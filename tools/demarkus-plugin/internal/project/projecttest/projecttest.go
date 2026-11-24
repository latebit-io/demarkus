// Package projecttest lays out git checkouts by hand, the way git writes
// them, so tests of project identity need no git binary.
package projecttest

import (
	"os"
	"path/filepath"
	"testing"
)

// Repo makes dir a main checkout whose local config is config.
func Repo(t *testing.T, dir, config string) {
	t.Helper()
	Write(t, filepath.Join(dir, ".git", "config"), config)
}

// Bare makes gitDir a repository with no checkout of its own.
func Bare(t *testing.T, gitDir, config string) {
	t.Helper()
	Write(t, filepath.Join(gitDir, "config"), config)
}

// Worktree links wt to the repository whose git dir is gitDir, as
// `git worktree add` records it.
func Worktree(t *testing.T, gitDir, wt string) {
	t.Helper()
	record := filepath.Join(gitDir, "worktrees", filepath.Base(wt))
	Write(t, filepath.Join(record, "commondir"), "../..\n")
	Write(t, filepath.Join(record, "gitdir"), filepath.Join(wt, ".git")+"\n")
	Write(t, filepath.Join(wt, ".git"), "gitdir: "+record+"\n")
}

// Origin is a local config naming url as the origin remote.
func Origin(url string) string {
	return "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
}

// Broken makes dir a checkout whose .git file is no pointer; body is what
// the file holds instead.
func Broken(t *testing.T, dir, body string) {
	t.Helper()
	Write(t, filepath.Join(dir, ".git"), body)
}

// Write puts body at path, making its directories.
func Write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
