package facts

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// ReadGit reads root, branch, HEAD and uncommitted paths (repo-relative) for
// the repository containing dir. dir "" is the process cwd.
// No optional locks: a hook must never hold index.lock against the user's git.
func ReadGit(ctx context.Context, dir string) (Git, error) {
	args := append(prefix(dir), "--no-optional-locks", "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=normal")
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary, no shell; dir is only ever -C's value
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Git{}, fmt.Errorf("git status in %q: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	g := parseStatus(out)
	stderr.Reset()
	root := exec.CommandContext(ctx, "git", append(prefix(dir), "rev-parse", "--show-toplevel")...) //nolint:gosec // as above
	root.Stderr = &stderr
	top, err := root.Output()
	if err != nil {
		return Git{}, fmt.Errorf("git rev-parse in %q: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	g.Root = strings.TrimSpace(string(top))
	return g, nil
}

func prefix(dir string) []string {
	if dir == "" {
		return nil
	}
	return []string{"-C", dir}
}

// parseStatus reads `git status --porcelain=v2 --branch -z` output.
func parseStatus(out []byte) Git {
	var g Git
	recs := strings.Split(string(out), "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		switch {
		case strings.HasPrefix(rec, "# branch.oid "):
			if oid := strings.TrimPrefix(rec, "# branch.oid "); oid != "(initial)" {
				g.Head = oid
			}
		case strings.HasPrefix(rec, "# branch.head "):
			if head := strings.TrimPrefix(rec, "# branch.head "); head != "(detached)" {
				g.Branch = head
			}
		case strings.HasPrefix(rec, "1 "):
			g.add(field(rec, 8))
		case strings.HasPrefix(rec, "2 "):
			g.add(field(rec, 9))
			i++ // the next record is the rename's original path
		case strings.HasPrefix(rec, "u "):
			g.add(field(rec, 10))
		case strings.HasPrefix(rec, "? "):
			g.add(rec[2:])
		}
	}
	return g
}

// field is the rest of rec after n space-separated fields; paths may hold spaces.
func field(rec string, n int) string {
	parts := strings.SplitN(rec, " ", n+1)
	if len(parts) <= n {
		return ""
	}
	return parts[n]
}

func (g *Git) add(path string) {
	if path != "" {
		g.Uncommitted = append(g.Uncommitted, path)
	}
}
