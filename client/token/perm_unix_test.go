//go:build unix

package token

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestAtomicWriteKeepsOwnerAndMode guards the installer's root:demarkus 0640
// layout: a rewrite as the caller's 0600 locks the server out of its tokens.
func TestAtomicWriteKeepsOwnerAndMode(t *testing.T) {
	entry := Entry{Hash: "sha256-bbb", Paths: []string{"/*"}, Operations: []string{"publish"}}
	writes := map[string]func(path string) error{
		"AppendEntry": func(path string) error { return AppendEntry(path, "second", &entry) },
		"WriteFile": func(path string) error {
			return WriteFile(path, File{Tokens: map[string]Entry{"second": entry}})
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.toml")
			seed := "[tokens.first]\nhash = \"sha256-aaa\"\npaths = [\"/*\"]\noperations = [\"read\"]\n"
			if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatalf("chmod seed: %v", err)
			}
			before := ownerOf(t, path)
			// A group the temp file would not get by default makes a skipped chown visible.
			if gid, ok := otherGroup(t, before.Gid); ok {
				if err := os.Chown(path, -1, gid); err != nil {
					t.Fatalf("chgrp seed: %v", err)
				}
				before = ownerOf(t, path)
			} else {
				t.Log("no supplementary group; gid preservation not exercised")
			}
			if err := write(path); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o640 {
				t.Errorf("mode after %s = %o, want 640", name, got)
			}
			after := ownerOf(t, path)
			if after.Uid != before.Uid || after.Gid != before.Gid {
				t.Errorf("owner after %s = %d:%d, want %d:%d", name, after.Uid, after.Gid, before.Uid, before.Gid)
			}
		})
	}

	t.Run("new file stays 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")
		if err := AppendEntry(path, "first", &entry); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("new file mode = %o, want 600", got)
		}
	})
}

func ownerOf(t *testing.T, path string) *syscall.Stat_t {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no owner info", path)
	}
	return st
}

// otherGroup returns a group of the caller other than current, if any.
func otherGroup(t *testing.T, current uint32) (int, bool) {
	t.Helper()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatalf("getgroups: %v", err)
	}
	for _, g := range groups {
		if uint32(g) != current {
			return g, true
		}
	}
	return 0, false
}
