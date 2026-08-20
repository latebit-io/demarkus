// Package registry owns every mutation of the shared ~/.demarkus + ~/.config/mcp
// state (soul catalog, project bindings, knowledge registry, policy mirrors,
// promote targets, MCP server config). Centralizing it here replaces the
// per-plugin bash (soul-join.sh, soul-default.sh, register-knowledge.sh,
// mirror-policy.sh, promote-target.sh) and the mcp-config.mjs JS — one
// implementation, called by every harness's thin adapter.
package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/lockdir"
)

// withLock runs fn while holding an atomic mkdir mutex for path+".lock".
// Bounded ~2s.
func withLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return lockdir.WithLock(path+".lock", 100, 20*time.Millisecond, fn)
}

// atomicWrite writes data to path via a temp file + rename (no torn writes).
func atomicWrite(path string, data []byte) error { return atomicWritePerm(path, data, 0o644) }

// atomicWritePerm is atomicWrite with an explicit mode. The temp file is created
// WITH that mode (via O_CREATE perm) so a secret (e.g. a 0600 token) is never
// briefly world-readable before a later chmod.
func atomicWritePerm(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// O_CREATE perm is masked by umask; enforce the exact mode for secrets.
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
