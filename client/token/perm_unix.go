//go:build unix

package token

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// matchExisting gives tmp the owner and mode of the file it replaces, so a
// sudo generate or revoke keeps the installer's root:demarkus 0640 readable by
// the server. Fails closed: a file the server cannot read would drop all auth.
func matchExisting(tmp, path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat tokens file %q: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat tokens file %q: no owner info", path)
	}
	if err := os.Chown(tmp, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("keep owner of tokens file %q: %w", path, err)
	}
	if err := os.Chmod(tmp, info.Mode().Perm()); err != nil {
		return fmt.Errorf("keep mode of tokens file %q: %w", path, err)
	}
	return nil
}
