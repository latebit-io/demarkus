package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	m := layerMap{
		{pattern: "protocol/...", layer: "protocol", line: 1},
		{pattern: "client/journal", layer: "model", line: 2},
		{pattern: "client/mcpfmt", layer: "surface", line: 3},
		{pattern: "client/fetch", layer: "storeclient", line: 4},
		{pattern: "client/gone", layer: "model", line: 5},
	}
	pkgs := []pkg{
		{path: "protocol", imports: nil},
		{path: "client/fetch", imports: []string{"protocol"}, testImports: []string{"client/journal"}},
		{path: "client/journal", imports: []string{"client/fetch", "client/mcpfmt"}, testImports: []string{"client/mcpfmt", "client/journal"}},
		{path: "client/mcpfmt", imports: []string{"client/journal", "client/new"}},
		{path: "client/new"},
	}
	r := check(m, pkgs)

	wantViolations := []string{
		"client/fetch (storeclient) -> client/journal (model) [test]",
		"client/journal (model) -> client/mcpfmt (surface)",
	}
	if !slices.Equal(r.violations, wantViolations) {
		t.Errorf("violations:\n%s\nwant:\n%s", strings.Join(r.violations, "\n"), strings.Join(wantViolations, "\n"))
	}
	if want := []string{"client/new"}; !slices.Equal(r.unmapped, want) {
		t.Errorf("unmapped = %v, want %v", r.unmapped, want)
	}
	if len(r.unused) != 1 || r.unused[0].pattern != "client/gone" {
		t.Errorf("unused = %v, want client/gone", r.unused)
	}
}

func TestDiff(t *testing.T) {
	added, removed := diff([]string{"a", "b"}, []string{"b", "c"})
	if !slices.Equal(added, []string{"c"}) || !slices.Equal(removed, []string{"a"}) {
		t.Fatalf("added %v, removed %v; want [c], [a]", added, removed)
	}
}

func TestParseList(t *testing.T) {
	out := repoPrefix + "client/journal\tfmt " + repoPrefix + "client/fetch\t" +
		repoPrefix + "client/fetchtest testing " + repoPrefix + "client/journal\n" +
		repoPrefix + "protocol\t\t \n"
	pkgs, err := parseList([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	got := pkgs[0]
	if got.path != "client/journal" || !slices.Equal(got.imports, []string{"client/fetch"}) ||
		!slices.Equal(got.testImports, []string{"client/fetchtest", "client/journal"}) {
		t.Errorf("parsed %+v", got)
	}
	if pkgs[1].path != "protocol" || len(pkgs[1].imports)+len(pkgs[1].testImports) != 0 {
		t.Errorf("parsed %+v", pkgs[1])
	}

	for _, bad := range []string{"example.com/x\t\t\n", repoPrefix + "x\tno test column\n"} {
		if _, err := parseList([]byte(bad)); err == nil {
			t.Errorf("parseList(%q) accepted a bad line", bad)
		}
	}
}
