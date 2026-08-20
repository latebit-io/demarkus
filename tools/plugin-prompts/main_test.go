package main

import (
	"slices"
	"strings"
	"testing"
)

func TestAddMarkerPreservesFrontmatter(t *testing.T) {
	got := string(addMarker([]byte("---\nname: demo\ndescription: test\n---\n\n# Demo\n")))
	want := "---\nname: demo\ndescription: test\n---\n" + generatedMarker + "\n\n# Demo\n"
	if got != want {
		t.Fatalf("addMarker() = %q, want %q", got, want)
	}
}

func TestForbiddenTermsAreHarnessSpecific(t *testing.T) {
	for _, test := range []struct {
		harness string
		term    string
	}{
		{harness: "claude", term: "OpenCode"},
		{harness: "pi", term: "AskUserQuestion"},
		{harness: "opencode", term: "mcp__"},
		{harness: "opencode", term: "OpenClaw"},
	} {
		t.Run(test.harness+"/"+test.term, func(t *testing.T) {
			if !contains(forbiddenTerms(test.harness), test.term) {
				t.Fatalf("%s forbidden terms do not contain %q", test.harness, test.term)
			}
		})
	}
}

func TestValidateArtifactRejectsForeignHarness(t *testing.T) {
	err := validateArtifact("commands/test.md", []byte(generatedMarker+"\nUse OpenClaw here.\n"), &target{Harness: "pi"})
	if err == nil || !strings.Contains(err.Error(), "OpenClaw") {
		t.Fatalf("validateArtifact() error = %v, want OpenClaw rejection", err)
	}
}

func TestRepositoryCorpusRendersAllArtifacts(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := renderAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 54 {
		t.Fatalf("renderAll() produced %d artifacts, want 54", len(artifacts))
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
