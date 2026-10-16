// Package reporoot finds the repository root for the tools that run from
// anywhere inside it.
package reporoot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Find walks up from the working directory to the first directory holding
// marker, a slash-separated path relative to the root.
func Find(marker string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		path := filepath.Join(dir, filepath.FromSlash(marker))
		_, err := os.Stat(path)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s not found in this directory or its parents", marker)
		}
		dir = parent
	}
}
