//go:build unix

package knowledgeseed

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO must be refused, not waited on: a blocking open would park the
// world open until a writer appeared, with no context to interrupt it.
func TestPolicySeedFromFileRefusesFIFOWithoutBlocking(t *testing.T) {
	name := filepath.Join(t.TempDir(), "policy.md")
	if err := syscall.Mkfifo(name, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := PolicySeedFromFile(name)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted a FIFO as a policy file")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PolicySeedFromFile blocked on a FIFO")
	}
}
