package reporoot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFind(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "marker.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(root, "a", "b"))

	got, err := Find("a/marker.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(root); mustEval(t, got) != want {
		t.Errorf("Find = %s, want %s", got, root)
	}
	if _, err := Find("absent.txt"); err == nil || !strings.Contains(err.Error(), "absent.txt not found") {
		t.Errorf("err = %v, want not found", err)
	}
}

// mustEval resolves symlinks such as macOS /var -> /private/var.
func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
