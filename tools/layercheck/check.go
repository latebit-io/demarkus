package main

import (
	"fmt"
	"slices"
)

// pkg is one package and the repo packages it imports.
type pkg struct {
	path        string
	imports     []string
	testImports []string // test files only, may repeat imports
}

type violation struct {
	from, fromLayer string
	to, toLayer     string
	testOnly        bool
}

func (v violation) String() string {
	s := fmt.Sprintf("%s (%s) -> %s (%s)", v.from, v.fromLayer, v.to, v.toLayer)
	if v.testOnly {
		s += " [test]"
	}
	return s
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

	found := map[string]bool{}
	for _, p := range pkgs {
		from, ok := layerOf(p.path)
		for _, edge := range edges(p) {
			to, toOK := layerOf(edge.to)
			if !ok || !toOK || mayImport(from, to) {
				continue
			}
			v := violation{from: p.path, fromLayer: from, to: edge.to, toLayer: to, testOnly: edge.testOnly}
			found[v.String()] = true
		}
	}

	for line := range found {
		r.violations = append(r.violations, line)
	}
	for path := range unmapped {
		r.unmapped = append(r.unmapped, path)
	}
	for _, e := range m {
		if !used[e.pattern] {
			r.unused = append(r.unused, e)
		}
	}
	slices.Sort(r.violations)
	slices.Sort(r.unmapped)
	return r
}

type edge struct {
	to       string
	testOnly bool
}

// edges lists each import of p once; a package production code imports is
// not test only, whatever its tests import.
func edges(p pkg) []edge {
	var out []edge
	for _, to := range p.imports {
		out = append(out, edge{to: to})
	}
	seen := map[string]bool{p.path: true} // an external test imports its own package
	for _, to := range p.testImports {
		if seen[to] || slices.Contains(p.imports, to) {
			continue
		}
		seen[to] = true
		out = append(out, edge{to: to, testOnly: true})
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
