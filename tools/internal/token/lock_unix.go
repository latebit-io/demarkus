package token

import (
	"os"
	"syscall"
)

// lockExclusive acquires an exclusive advisory lock on f via flock(2).
// The lock is released when f is closed.
//
// demarkus targets linux and darwin only (see goreleaser). The package
// is *nix only.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
