// Package registrytest holds the fixtures the registry packages' tests share:
// a temp home, staged catalog rows, injected write failures, captured stderr.
package registrytest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/statefile"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// SetupHome points HOME at a temp dir with an empty ~/.demarkus.
func SetupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".demarkus"), 0o750); err != nil {
		t.Fatal(err)
	}
	return home
}

// RegisterRow appends a catalog record the way an older plugin or a hand edit
// would, so tests can stage rows a join never writes (external token files).
func RegisterRow(t *testing.T, row config.MemoryRow) {
	t.Helper()
	p, err := config.StatePath("souls")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(row.Record() + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// FailWritesTo makes every state write to a file named name fail for the test.
func FailWritesTo(t *testing.T, name string) {
	t.Helper()
	originalWriter := statefile.Writer
	t.Cleanup(func() { statefile.Writer = originalWriter })
	statefile.Writer = func(path string, data []byte, mode os.FileMode) error {
		if filepath.Base(path) == name {
			return errors.New("injected write failure")
		}
		return statefile.WriteFile(path, data, mode)
	}
}

// CaptureStderr runs fn with stderr redirected and returns what it wrote.
func CaptureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = stderr
	callErr := fn()
	os.Stderr = originalStderr
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	warning, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(warning), callErr
}
