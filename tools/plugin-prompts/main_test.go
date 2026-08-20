package main

import (
	"slices"
	"strings"
	"testing"
)

func TestAddMarkerPreservesFrontmatter(t *testing.T) {
	got := string(addMarker([]byte("---\nname: demo\ndescription: test\n---\n\n# Demo\n")))
	want := "---\nname: demo\ndescription: test\n---\n" + markdownlintDirective + "\n" + generatedMarker + "\n\n# Demo\n"
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

func TestDecodeManifestRejectsUnknownField(t *testing.T) {
	_, err := decodeManifest([]byte(`{"targets": [], "unexpected": true}`))
	if err == nil {
		t.Fatal("decodeManifest() accepted an unknown field")
	}
}

func TestValidateManifestRejectsIncompleteTarget(t *testing.T) {
	spec := validManifest()
	spec.Targets[0].ToolForm = ""
	if err := validateManifest(&spec); err == nil {
		t.Fatal("validateManifest() accepted an empty tool_form")
	}
}

func TestValidateManifestRejectsDuplicatePair(t *testing.T) {
	spec := validManifest()
	spec.Targets[1].Surface = spec.Targets[0].Surface
	spec.Targets[1].Harness = spec.Targets[0].Harness
	if err := validateManifest(&spec); err == nil {
		t.Fatal("validateManifest() accepted a duplicate surface/harness pair")
	}
}

func TestValidateManifestRejectsDuplicateOutput(t *testing.T) {
	spec := validManifest()
	spec.Targets[1].Output = spec.Targets[0].Output
	if err := validateManifest(&spec); err == nil {
		t.Fatal("validateManifest() accepted a duplicate output")
	}
}

func validManifest() manifest {
	harnesses := []string{"claude", "pi", "opencode"}
	surfaces := []string{"memory", "knowledge"}
	spec := manifest{}
	for _, surface := range surfaces {
		for _, harness := range harnesses {
			name := surface + "-" + harness
			spec.Targets = append(spec.Targets, target{
				Name:             name,
				Surface:          surface,
				Harness:          harness,
				Agent:            harness,
				Output:           "plugins/" + name,
				ProjectDir:       "project",
				RepoInstructions: "instructions",
				ToolForm:         "tools",
			})
		}
	}
	return spec
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
