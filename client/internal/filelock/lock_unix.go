//go:build unix

package filelock

import (
	"os"
	"syscall"
)

// lockExclusive takes flock(2) on f, released when f closes. Unix only by
// design: a platform without it fails to compile at the call site.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
