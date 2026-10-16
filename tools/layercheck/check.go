package main

import (
	"fmt"
	"maps"
	"slices"
)

// pkg is one package and the repo packages it imports.
type pkg struct {
	path        string
	imports     []string
	testImports []string // test files only, may repeat imports
}

type report struct {
	violations []string // sorted, one line per upward import
	unmapped   []string // packages no entry covers
	unused     []entry  // entries that map no package
}

func check(m layerMap, pkgs []pkg) report {
	var r report
	used := map[string]bool{}
	unmapped := map[string]bool{}
	layerOf := func(path string) (string, bool) {
		e, ok := m.resolve(path)
		if !ok {
			unmapped[path] = true
			return "", false
		}
		used[e.pattern] = true
		return e.layer, true
	}

	// Package paths are unique and edges lists each import once, so every
	// line is too.
	for _, p := range pkgs {
		from, ok := layerOf(p.path)
		for _, e := range edges(p) {
			to, toOK := layerOf(e.to)
			if !ok || !toOK || mayImport(from, to) {
				continue
			}
			line := fmt.Sprintf("%s (%s) -> %s (%s)", p.path, from, e.to, to)
			if e.testOnly {
				line += " [test]"
			}
			r.violations = append(r.violations, line)
		}
	}
	slices.Sort(r.violations)
	r.unmapped = slices.Sorted(maps.Keys(unmapped))
	for _, e := range m {
		if !used[e.pattern] {
			r.unused = append(r.unused, e)
		}
	}
	return r
}

type edge struct {
	to       string
	testOnly bool
}

// edges lists each import of p once; a package production code imports is
// not test only, whatever its tests import.
func edges(p pkg) []edge {
	seen := map[string]bool{p.path: true} // an external test imports its own package
	var out []edge
	for _, to := range p.imports {
		seen[to] = true
		out = append(out, edge{to: to})
	}
	for _, to := range p.testImports {
		if !seen[to] {
			seen[to] = true
			out = append(out, edge{to: to, testOnly: true})
		}
	}
	return out
}

// diff compares the current violations with the baseline: added are new
// upward imports, removed are fixed ones the baseline still lists.
func diff(baseline, current []string) (added, removed []string) {
	for _, line := range current {
		if !slices.Contains(baseline, line) {
			added = append(added, line)
		}
	}
	for _, line := range baseline {
		if !slices.Contains(current, line) {
			removed = append(removed, line)
		}
	}
	return added, removed
}
