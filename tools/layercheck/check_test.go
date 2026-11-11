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
		{
			path: "x/model", imports: []string{"x/client", "x/surface"}, testImports: []string{"x/surface", "x/model"},
			external: []string{mcpSDK + "/mcp", "example.com/free"},
		},
		{path: "x/surface", imports: []string{"x/model", "x/new"}, external: []string{mcpSDK + "/mcp"}},
		{path: "x/new"},
	}
	r := check(m, pkgs)

	wantViolations := []string{
		"x/client (storeclient) -> x/model (model) [test]",
		"x/model (model) -> " + mcpSDK + "/mcp (external)",
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
	if len(got.external) != 0 {
		t.Errorf("standard library read as an outside module: %v", got.external)
	}
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

func TestMergePackages(t *testing.T) {
	got := mergePackages([]pkg{
		{path: "x/b", imports: []string{"x/a"}, external: []string{"example.com/unix"}},
		{path: "x/a"},
		{path: "x/b", imports: []string{"x/a", "x/win"}, testImports: []string{"x/t"}},
	})
	if len(got) != 2 || got[0].path != "x/a" || got[1].path != "x/b" {
		t.Fatalf("merged %+v", got)
	}
	b := got[1]
	if !slices.Equal(b.imports, []string{"x/a", "x/win"}) || !slices.Equal(b.testImports, []string{"x/t"}) ||
		!slices.Equal(b.external, []string{"example.com/unix"}) {
		t.Errorf("merged %+v", b)
	}
}

func TestOutsideModules(t *testing.T) {
	got := outsideModules("fmt net/http " + repoPrefix + "x/a example.com/m/v2 golang.org/x/sync")
	if want := []string{"example.com/m/v2", "golang.org/x/sync"}; !slices.Equal(got, want) {
		t.Errorf("outsideModules = %v, want %v", got, want)
	}
}

func TestMayUse(t *testing.T) {
	for _, c := range []struct {
		layer, path string
		want        bool
	}{
		{"model", mcpSDK + "/mcp", false},
		{"model", mcpSDK, false},
		{"model", mcpSDK + "x/mcp", true},
		{"surface", mcpSDK + "/mcp", true},
		{"store", "cloud.google.com/go/storage", true},
		{"storeclient", "cloud.google.com/go/storage", false},
		{"knowledge", "k8s.io/client-go/rest", true},
		{"soul", "k8s.io/client-go/rest", false},
		{"knowledge", "charm.land/bubbletea/v2", false},
		{"tool", "cloud.google.com/go/storage", true},
		{"a-new-layer", "cloud.google.com/go/storage", false}, // denied until named
		{"model", "golang.org/x/sync/errgroup", true},
	} {
		if got := mayUse(c.layer, c.path); got != c.want {
			t.Errorf("mayUse(%s, %s) = %v, want %v", c.layer, c.path, got, c.want)
		}
	}
}
