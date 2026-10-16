// Command layercheck fails any import that points up the target layers, a
// package layer-map.txt does not map, and a map line that maps nothing.
// Upward imports that predate it sit in layer-ratchet.txt, which only shrinks.
//
// Usage: layercheck [-update]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	mapFile      = "layer-map.txt"
	baselineFile = "layer-ratchet.txt"
)

const baselineHeader = `# Upward imports that predate the layer guard. tools/layercheck fails a new
# one, and a fixed one still listed: run (cd tools && go run ./layercheck -update).
`

func main() {
	update := flag.Bool("update", false, "rewrite "+baselineFile+" to the current upward imports")
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	root, err := findRoot()
	if err == nil {
		err = run(root, *update)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "layercheck:", err)
		os.Exit(1)
	}
}

func run(root string, update bool) error {
	f, err := os.Open(filepath.Join(root, mapFile))
	if err != nil {
		return err
	}
	m, err := parseMap(f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", mapFile, err)
	}
	pkgs, err := listPackages(root)
	if err != nil {
		return err
	}
	r := check(m, pkgs)
	if err := mapProblems(r); err != nil {
		return err
	}

	baselinePath := filepath.Join(root, baselineFile)
	if update {
		var body strings.Builder
		body.WriteString(baselineHeader)
		for _, line := range r.violations {
			body.WriteString(line + "\n")
		}
		if err := os.WriteFile(baselinePath, []byte(body.String()), 0o600); err != nil {
			return err
		}
		fmt.Printf("layercheck: %s updated (%d entries)\n", baselineFile, len(r.violations))
		return nil
	}
	baseline, err := readBaseline(baselinePath)
	if err != nil {
		return err
	}
	added, removed := diff(baseline, r.violations)
	for _, line := range added {
		fmt.Println("+", line)
	}
	for _, line := range removed {
		fmt.Println("-", line)
	}
	if len(added)+len(removed) > 0 {
		return fmt.Errorf("upward imports differ from %s: '+' points up, fix it; '-' is fixed, run with -update", baselineFile)
	}
	fmt.Printf("✓ layers hold (%d packages, %d baseline entries)\n", len(pkgs), len(baseline))
	return nil
}

// mapProblems reports packages the map misses and lines that map nothing;
// both block an update, so the baseline never records a stale map.
func mapProblems(r report) error {
	for _, path := range r.unmapped {
		fmt.Printf("%s: no layer; add it to %s\n", path, mapFile)
	}
	for _, e := range r.unused {
		fmt.Printf("%s:%d: %s maps no package; remove or fix it\n", mapFile, e.line, e.pattern)
	}
	if len(r.unmapped)+len(r.unused) > 0 {
		return fmt.Errorf("%s does not match the packages", mapFile)
	}
	return nil
}

func readBaseline(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		_, err := os.Stat(filepath.Join(dir, mapFile))
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(mapFile + " not found in this directory or its parents")
		}
		dir = parent
	}
}
