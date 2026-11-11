// Package filelock serializes the read-modify-write of a file several
// processes share: the tokens files, the graph store.
package filelock

import (
	"errors"
	"fmt"
	"os"
)

// With runs fn under an exclusive advisory lock on `<path>.lock`. A sidecar,
// so taking the lock never creates the data file. It holds between processes
// on one host; a filesystem without working flock (NFS) gets none.
func With(path string, fn func() error) (err error) {
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	defer func() {
		if cerr := lf.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close lock file %q: %w", lockPath, cerr))
		}
	}()
	if err := lockExclusive(lf); err != nil {
		return fmt.Errorf("acquire lock on %q: %w", lockPath, err)
	}
	return fn()
}
