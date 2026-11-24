package project

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Where a slug came from.
const (
	SourceConfig = "config" // the repository's demarkus.project
	SourceOrigin = "origin" // the origin remote's repository name
	SourceMain   = "main"   // a linked worktree's main checkout name
	SourceDir    = "dir"    // the directory basename
)

// IdentityError reports a checkout whose git files cannot name its project.
// Reason is fixed text: those files are repository content, never echoed.
type IdentityError struct {
	Reason     string
	Unreadable bool // a read failed: the checkout may be sound, it could not be seen
}

func (e *IdentityError) Error() string {
	return "git cannot identify this checkout: " + e.Reason + "; ask the user which project"
}

// unidentified logs the read failure here, once: every surface prints only
// the fixed reason.
func unidentified(reason string, cause error) *IdentityError {
	if cause != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] project: "+reason+": "+cause.Error())
	}
	return &IdentityError{Reason: reason, Unreadable: cause != nil}
}

// identify names dir's project from git's own files, never by running git:
// a hook has no PATH, timeout or environment to trust. Only a checkout's top
// level takes the repository's name; any other directory keeps its basename.
func identify(dir string, co checkout) (slug, source string, err error) {
	if co.top != dir {
		slug, err = Slug(dir)
		return slug, SourceDir, err
	}
	vals, err := readGitConfig(filepath.Join(co.common, "config"))
	if err != nil {
		return "", "", unidentified("its repository config is unreadable", err)
	}
	if vals.hasOverride {
		if !IsSlug(vals.override) {
			return "", "", unidentified("its demarkus.project git config is not a valid slug", nil)
		}
		return vals.override, SourceConfig, nil
	}
	if name := repoName(vals.origin); IsSlug(name) {
		return name, SourceOrigin, nil
	}
	if co.linked {
		if name := normalize(strings.TrimSuffix(filepath.Base(co.anchor), ".git")); IsSlug(name) {
			return name, SourceMain, nil
		}
	}
	slug, err = Slug(dir)
	return slug, SourceDir, err
}

// repoName is the repository name a remote URL ends in, for the URL, scp
// (host:org/repo) and plain path spellings alike.
func repoName(url string) string {
	s := strings.TrimRight(strings.TrimSpace(url), "/")
	s = strings.TrimRight(strings.TrimSuffix(s, ".git"), "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return normalize(s)
}

// checkout is where a directory sits in git; the zero value is outside any.
type checkout struct {
	top    string // nearest directory holding a .git entry, as dir spells it
	common string // the repository's shared git directory
	anchor string // main checkout; a bare repository's git dir
	linked bool   // a linked worktree
}

// upTo yields dir and its ancestors, nearest first, no higher than stop
// ("" for the filesystem root).
func upTo(dir, stop string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for cur := dir; yield(cur) && cur != stop; {
			parent := filepath.Dir(cur)
			if parent == cur {
				return
			}
			cur = parent
		}
	}
}

// findCheckout walks dir and its ancestors for a .git entry; a missing
// directory is outside any checkout.
func findCheckout(dir string) (checkout, error) {
	for cur := range upTo(dir, "") {
		// dir may come from a repository's worktree record: only looked at, and
		// trusted only once its own pointer names that repository.
		fi, err := os.Stat(filepath.Join(cur, ".git")) //nolint:gosec // see above
		if err == nil {
			return readCheckout(cur, fi)
		}
		// A directory we may not search (a sandboxed ancestor) holds no
		// checkout we could read; only a real read failure is surfaced.
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) && !errors.Is(err, syscall.ENOTDIR) {
			return checkout{}, unidentified("its .git entry is unreadable", err)
		}
	}
	return checkout{}, nil
}

// readCheckout follows git's own discovery: a .git directory is the
// repository; a .git file points at a git dir, and a commondir file inside
// that marks a linked worktree.
func readCheckout(top string, entry fs.FileInfo) (checkout, error) {
	if entry.IsDir() {
		return checkout{top: top, common: filepath.Join(top, ".git"), anchor: top}, nil
	}
	pointer, err := os.ReadFile(filepath.Join(top, ".git")) //nolint:gosec // as findCheckout
	if err != nil {
		return checkout{}, unidentified("its .git file is unreadable", err)
	}
	gitDir, found := strings.CutPrefix(strings.TrimSpace(string(pointer)), "gitdir:")
	if gitDir = strings.TrimSpace(gitDir); !found || gitDir == "" {
		return checkout{}, unidentified("its .git file is malformed", nil)
	}
	gitDir = absFrom(top, gitDir)
	// gitDir is the checkout's own pointer: following it is the job. What it
	// yields is two names and never echoed; a borrowed repository is refused below.
	switch fi, err := os.Stat(gitDir); { //nolint:gosec // see above
	case err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
		return checkout{}, unidentified("its git directory is unreadable", err)
	case err != nil || !fi.IsDir():
		return checkout{}, unidentified("its .git file names a missing git directory", nil)
	}
	shared, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // as above
	if errors.Is(err, fs.ErrNotExist) {
		// A submodule or a separate git dir: its own repository.
		return checkout{top: top, common: gitDir, anchor: top}, nil
	}
	if err != nil {
		return checkout{}, unidentified("its worktree record is unreadable", err)
	}
	common := absFrom(gitDir, strings.TrimSpace(string(shared)))
	// A worktree's git dir lives in its repository; a pointer anywhere else
	// would borrow another repository's name and binding.
	if !sameDir(filepath.Dir(gitDir), filepath.Join(common, "worktrees")) {
		return checkout{}, unidentified("its worktree record is outside its repository", nil)
	}
	anchor, err := mainCheckout(common)
	if err != nil {
		return checkout{}, err
	}
	return checkout{top: top, common: common, anchor: anchor, linked: true}, nil
}

// mainCheckout is where a linked worktree's repository is checked out: beside
// a dot-named git dir, at core.worktree, or the git dir itself when bare.
func mainCheckout(common string) (string, error) {
	if strings.HasPrefix(filepath.Base(common), ".") {
		return filepath.Dir(common), nil
	}
	vals, err := readGitConfig(filepath.Join(common, "config"))
	switch {
	case err != nil:
		return "", unidentified("its repository config is unreadable", err)
	case vals.worktree != "":
		return absFrom(common, vals.worktree), nil
	case vals.bare:
		return common, nil
	}
	// A git dir kept apart records no way back to its checkout, and git
	// reports the git dir. Naming that would give one repository two anchors.
	return "", unidentified("its repository's main checkout cannot be found", nil)
}

// worktrees are the tops of the linked worktrees of co's repository. Each is
// confirmed by its own pointer: a record alone could name any directory. A
// failed read is an error: a list cut short would pass for complete.
func worktrees(co checkout) ([]string, error) {
	records := filepath.Join(co.common, "worktrees")
	entries, err := os.ReadDir(records)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the repository's worktrees: %w", err)
	}
	var tops []string
	for _, e := range entries {
		record := filepath.Join(records, e.Name())
		back, err := os.ReadFile(filepath.Join(record, "gitdir")) //nolint:gosec // the repository's own record
		if gone(err) {
			continue // a record being pruned names no worktree
		}
		if err != nil {
			return nil, fmt.Errorf("read the repository's worktree record: %w", err)
		}
		top := filepath.Dir(absFrom(record, strings.TrimSpace(string(back))))
		wt, err := checkoutAt(top)
		if idErr, ok := errors.AsType[*IdentityError](err); ok && idErr.Unreadable {
			return nil, fmt.Errorf("a worktree of this repository cannot be read (%s); fix its permissions or run git worktree prune", idErr.Reason)
		}
		// A pointer that is gone, broken or another repository's is no worktree of this one.
		if err == nil && wt.linked && sameDir(wt.common, co.common) {
			tops = append(tops, top)
		}
	}
	return tops, nil
}

// checkoutAt reads the checkout whose top is exactly top; the zero value when
// top holds no .git entry. Unlike findCheckout it never looks above top.
func checkoutAt(top string) (checkout, error) {
	fi, err := os.Stat(filepath.Join(top, ".git")) //nolint:gosec // a recorded worktree, confirmed by readCheckout
	if gone(err) {
		return checkout{}, nil
	}
	if err != nil {
		return checkout{}, unidentified("its .git entry is unreadable", err)
	}
	return readCheckout(top, fi)
}

// gone reports a path that is not there, as opposed to one that cannot be read.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func absFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// sameDir reports two spellings of one directory on disk.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ia, errA := os.Stat(a) //nolint:gosec // the pointer's two ends, compared and nothing more
	ib, errB := os.Stat(b) //nolint:gosec // as above
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
