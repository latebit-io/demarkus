package statefile

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestAtomicWritePermConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	const writers = 32
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			<-start
			errs <- WriteFile(path, []byte("writer-"+strconv.Itoa(i)), 0o600)
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(body), "writer-") {
		t.Fatalf("final state: body=%q err=%v", body, err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".state.*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files: %v err=%v", temps, err)
	}
}
