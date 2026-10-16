package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

const repoPrefix = "github.com/latebit-io/demarkus/"

const listFormat = `{{.ImportPath}}{{"\t"}}{{join .Imports " "}}{{"\t"}}{{join .TestImports " "}} {{join .XTestImports " "}}`

// listPackages lists every package of the top-level modules; nested go.mod
// files (benchmark artifacts) stay out, as in scripts/lint-ratchet.sh.
func listPackages(root string) ([]pkg, error) {
	mods, err := filepath.Glob(filepath.Join(root, "*", "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("find modules: %w", err)
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("no go.mod under %s/*", root)
	}
	var pkgs []pkg
	for _, mod := range mods {
		dir := filepath.Dir(mod)
		cmd := exec.Command("go", "list", "-f", listFormat, "./...")
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
		}
		listed, err := parseList(out)
		if err != nil {
			return nil, fmt.Errorf("go list in %s: %w", dir, err)
		}
		pkgs = append(pkgs, listed...)
	}
	return pkgs, nil
}

// parseList reads listFormat output into repo-relative packages, keeping
// only imports of repo packages.
func parseList(out []byte) ([]pkg, error) {
	var pkgs []pkg
	for line := range strings.Lines(string(out)) {
		cols := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(cols) != 3 {
			return nil, fmt.Errorf("unexpected go list line %q", line)
		}
		path, ok := strings.CutPrefix(cols[0], repoPrefix)
		if !ok {
			return nil, fmt.Errorf("package %s is outside %s", cols[0], repoPrefix)
		}
		pkgs = append(pkgs, pkg{path: path, imports: repoImports(cols[1]), testImports: repoImports(cols[2])})
	}
	return pkgs, nil
}

func repoImports(field string) []string {
	var out []string
	for imp := range strings.FieldsSeq(field) {
		if rel, ok := strings.CutPrefix(imp, repoPrefix); ok {
			out = append(out, rel)
		}
	}
	return out
}
