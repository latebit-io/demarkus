package filelock

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A read-modify-write under the lock loses no update, and taking the lock
// never creates the file it guards.
func TestWithSerializesReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	if err := With(path, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("taking the lock created the data file: %v", err)
	}
	const writers = 32
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			err := With(path, func() error {
				data, err := os.ReadFile(path)
				if err != nil && !os.IsNotExist(err) {
					return err
				}
				n, _ := strconv.Atoi(strings.TrimSpace(string(data))) // absent or empty counts from zero
				return os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600)
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if data, err := os.ReadFile(path); err != nil || string(data) != strconv.Itoa(writers) {
		t.Errorf("counter = %q, %v; want %d", data, err, writers)
	}
}
