package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	m := layerMap{
		{pattern: "x/wire/...", layer: "protocol", line: 1},
		{pattern: "x/model", layer: "model", line: 2},
		{pattern: "x/surface", layer: "surface", line: 3},
		{pattern: "x/client", layer: "storeclient", line: 4},
		{pattern: "x/gone", layer: "model", line: 5},
	}
	pkgs := []pkg{
		{path: "x/wire", imports: nil},
		{path: "x/client", imports: []string{"x/wire"}, testImports: []string{"x/model"}},
		{path: "x/model", imports: []string{"x/client", "x/surface"}, testImports: []string{"x/surface", "x/model"}},
		{path: "x/surface", imports: []string{"x/model", "x/new"}},
		{path: "x/new"},
	}
	r := check(m, pkgs)

	wantViolations := []string{
		"x/client (storeclient) -> x/model (model) [test]",
		"x/model (model) -> x/surface (surface)",
	}
	if !slices.Equal(r.violations, wantViolations) {
		t.Errorf("violations:\n%s\nwant:\n%s", strings.Join(r.violations, "\n"), strings.Join(wantViolations, "\n"))
	}
	if want := []string{"x/new"}; !slices.Equal(r.unmapped, want) {
		t.Errorf("unmapped = %v, want %v", r.unmapped, want)
	}
	if len(r.unused) != 1 || r.unused[0].pattern != "x/gone" {
		t.Errorf("unused = %v, want x/gone", r.unused)
	}
}

func TestDiff(t *testing.T) {
	added, removed := diff([]string{"a", "b"}, []string{"b", "c"})
	if !slices.Equal(added, []string{"c"}) || !slices.Equal(removed, []string{"a"}) {
		t.Fatalf("added %v, removed %v; want [c], [a]", added, removed)
	}
}

func TestParseList(t *testing.T) {
	out := repoPrefix + "x/model\tfmt " + repoPrefix + "x/client\t" +
		repoPrefix + "x/clienttest testing " + repoPrefix + "x/model\n" +
		repoPrefix + "x/wire\t\t \n"
	pkgs, err := parseList([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("got %d packages, want 2", len(pkgs))
	}
	got := pkgs[0]
	if got.path != "x/model" || !slices.Equal(got.imports, []string{"x/client"}) ||
		!slices.Equal(got.testImports, []string{"x/clienttest", "x/model"}) {
		t.Errorf("parsed %+v", got)
	}
	if pkgs[1].path != "x/wire" || len(pkgs[1].imports)+len(pkgs[1].testImports) != 0 {
		t.Errorf("parsed %+v", pkgs[1])
	}

	for _, bad := range []string{"example.com/x\t\t\n", repoPrefix + "x\tno test column\n"} {
		if _, err := parseList([]byte(bad)); err == nil {
			t.Errorf("parseList(%q) accepted a bad line", bad)
		}
	}
}
