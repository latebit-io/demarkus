package main

import (
	"os"
	"path/filepath"
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
	if len(artifacts) != 81 {
		t.Fatalf("renderAll() produced %d artifacts, want 81", len(artifacts))
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
	if err := validateManifest(&spec); err == nil || !strings.Contains(err.Error(), "duplicate output") {
		t.Fatalf("validateManifest() error = %v, want duplicate output rejection", err)
	}
}

func TestValidateManifestRejectsInvalidOutput(t *testing.T) {
	for name, output := range map[string]string{
		"noncanonical": "plugins/pi-memory-typo",
		"absolute":     "/tmp/pi-memory",
		"traversal":    "plugins/../pi-memory",
	} {
		t.Run(name, func(t *testing.T) {
			spec := validManifest()
			spec.Targets[1].Output = output
			if err := validateManifest(&spec); err == nil {
				t.Fatalf("validateManifest() accepted output %q", output)
			}
		})
	}
}

func TestCheckAllReturnsArtifactReadError(t *testing.T) {
	root := t.TempDir()
	artifactPath := filepath.Join(root, "artifact.md")
	if err := os.Mkdir(artifactPath, 0o755); err != nil {
		t.Fatal(err)
	}
	err := checkAll(root, []artifact{{Path: artifactPath, Content: []byte("body")}})
	if err == nil || !strings.Contains(err.Error(), "read generated artifact") {
		t.Fatalf("checkAll() error = %v, want artifact read error", err)
	}
}

func TestCheckAllReportsMissingArtifactAsDrift(t *testing.T) {
	root := t.TempDir()
	output := "plugins/test"
	for _, subtree := range []string{"commands", "context", "skills"} {
		if err := os.MkdirAll(filepath.Join(root, output, subtree), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := &target{Output: output}
	artifactPath := filepath.Join(root, output, "commands", "missing.md")
	err := checkAll(root, []artifact{{Path: artifactPath, Content: []byte("body"), Target: target}})
	if err == nil || !strings.Contains(err.Error(), "generated plugin prompts are stale") {
		t.Fatalf("checkAll() error = %v, want stale artifact error", err)
	}
}

func validManifest() manifest {
	harnesses := []string{"claude", "pi", "opencode"}
	surfaces := []string{"memory", "knowledge"}
	spec := manifest{}
	for _, surface := range surfaces {
		for _, harness := range harnesses {
			name := surface + "-" + harness
			pair := surface + "/" + harness
			spec.Targets = append(spec.Targets, target{
				Name:             name,
				Surface:          surface,
				Harness:          harness,
				Agent:            harness,
				Output:           canonicalOutputs[pair],
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

func TestRenderAliasShipsTargetBodyWithDeprecatedDescription(t *testing.T) {
	dir := t.TempDir()
	commands := filepath.Join(dir, "commands")
	if err := os.MkdirAll(commands, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := "---\ndescription: Do the thing\n---\n\nBody for {{.Agent}}.\n"
	if err := os.WriteFile(filepath.Join(commands, "memory-x.md.tmpl"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(commands, "soul-x.md.alias")
	if err := os.WriteFile(alias, []byte("memory-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := renderAlias(alias, &target{Agent: "Claude Code"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"description: Deprecated alias of /memory-x. Do the thing\n", "Body for Claude Code.", generatedMarker} {
		if !strings.Contains(text, want) {
			t.Fatalf("alias output missing %q:\n%s", want, text)
		}
	}
	if _, err := renderAlias(filepath.Join(commands, "bare.alias"), &target{Agent: "Claude Code"}); err == nil {
		t.Fatal("alias without .md.alias suffix should be rejected")
	}
	self := filepath.Join(commands, "memory-x.md.alias")
	if err := os.WriteFile(self, []byte("memory-x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderAlias(self, &target{Agent: "Claude Code"}); err == nil {
		t.Fatal("self-alias should be rejected")
	}
}
