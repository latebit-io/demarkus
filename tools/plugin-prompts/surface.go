package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

var toolNamePattern = regexp.MustCompile(`\bmark_[a-z_]+\b`)

// checkToolSurface fails when a prompt template references a tool outside
// the lean profile, which is the surface the plugin launcher exposes.
func checkToolSurface(root string) error {
	var offenders []string
	source := filepath.Join(root, "plugins", "prompt-source")
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, name := range toolNamePattern.FindAllString(line, -1) {
				if mcpfmt.AdvancedTools[name] {
					offenders = append(offenders, fmt.Sprintf("%s:%d references %s", filepath.ToSlash(rel), i+1, name))
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan prompt source: %w", err)
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		return fmt.Errorf("prompt templates reference tools outside the lean profile:\n  %s\nadd the tool to the lean profile or drop the reference", strings.Join(offenders, "\n  "))
	}
	return nil
}
