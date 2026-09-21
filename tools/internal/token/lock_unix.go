//go:build unix

package token

import (
	"os"
	"syscall"
)

// lockExclusive acquires an exclusive advisory lock on f via flock(2).
// The lock is released when f is closed.
//
// The //go:build unix constraint scopes this file to unix-likes (linux,
// darwin, freebsd, etc.). demarkus only ships linux/darwin per
// goreleaser; non-unix builds exclude this file and lockExclusive
// becomes an undefined-symbol compile error at the call site, which is
// the intended signal.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
