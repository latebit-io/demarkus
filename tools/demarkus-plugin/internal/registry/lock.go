// Package registry owns every mutation of the shared ~/.demarkus + ~/.config/mcp
// state (memory catalog, project bindings, knowledge registry, policy mirrors,
// promote targets, MCP server config). Centralizing it here replaces the
// per-plugin bash (memory-join.sh, memory-default.sh, register-knowledge.sh,
// mirror-policy.sh, promote-target.sh) and the mcp-config.mjs JS — one
// implementation, called by every harness's thin adapter.
package registry

import (
	"errors"
	"os"
	"path/filepath"
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

// atomicWritePerm is atomicWrite with an explicit mode. CreateTemp starts at
// 0600, so secrets are never briefly world-readable before the exact chmod.
func atomicWritePerm(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	// O_CREATE perm is masked by umask; enforce the exact mode for secrets.
	if err := os.Chmod(tmp, perm); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return nil
}
