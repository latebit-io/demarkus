package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const repoPrefix = "github.com/latebit-io/demarkus/"

const listFormat = `{{.ImportPath}}{{"\t"}}{{join .Imports " "}}{{"\t"}}{{join .TestImports " "}} {{join .XTestImports " "}}`

// Every build the repo ships or tests: a file behind one of these constraints
// imports like any other. A new build tag joins buildTags.
var platforms = []string{"linux", "darwin", "windows"}

const buildTags = "gcsintegration"

// listPackages lists every package of the top-level modules on every
// platform; nested go.mod files (benchmark artifacts) stay out, as in
// scripts/lint-ratchet.sh.
func listPackages(root string) ([]pkg, error) {
	mods, err := filepath.Glob(filepath.Join(root, "*", "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("find modules: %w", err)
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("no go.mod under %s/*", root)
	}
	var listed []pkg
	for _, mod := range mods {
		for _, goos := range platforms {
			one, err := listModule(filepath.Dir(mod), goos)
			if err != nil {
				return nil, err
			}
			listed = append(listed, one...)
		}
	}
	return mergePackages(listed), nil
}

func listModule(dir, goos string) ([]pkg, error) {
	cmd := exec.Command("go", "list", "-tags", buildTags, "-f", listFormat, "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list in %s for %s: %w: %s", dir, goos, err, strings.TrimSpace(stderr.String()))
	}
	pkgs, err := parseList(out)
	if err != nil {
		return nil, fmt.Errorf("go list in %s for %s: %w", dir, goos, err)
	}
	return pkgs, nil
}

// mergePackages folds the listings of one package on several platforms into
// one, with the union of their imports, ordered by path.
func mergePackages(listed []pkg) []pkg {
	byPath := map[string]*pkg{}
	for _, p := range listed {
		into, ok := byPath[p.path]
		if !ok {
			byPath[p.path] = &pkg{path: p.path}
			into = byPath[p.path]
		}
		into.imports = union(into.imports, p.imports)
		into.testImports = union(into.testImports, p.testImports)
		into.external = union(into.external, p.external)
	}
	out := make([]pkg, 0, len(byPath))
	for _, p := range byPath {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b pkg) int { return strings.Compare(a.path, b.path) })
	return out
}

func union(have, more []string) []string {
	for _, s := range more {
		if !slices.Contains(have, s) {
			have = append(have, s)
		}
	}
	return have
}

// parseList reads listFormat output into repo-relative packages: imports of
// repo packages, and for production code the outside modules too.
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
		pkgs = append(pkgs, pkg{
			path: path, imports: repoImports(cols[1]), testImports: repoImports(cols[2]),
			external: outsideModules(cols[1]),
		})
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

// outsideModules keeps the imports that live in another module: not the repo,
// and not the standard library, whose first element has no dot.
func outsideModules(field string) []string {
	var out []string
	for imp := range strings.FieldsSeq(field) {
		first, _, _ := strings.Cut(imp, "/")
		if !strings.HasPrefix(imp, repoPrefix) && strings.Contains(first, ".") {
			out = append(out, imp)
		}
	}
	return out
}
