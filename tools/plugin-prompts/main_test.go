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
	// 54 rendered canonical prompts plus each brand's share,
	// derived from the manifest and source tree rather than from the render.
	want := 54 + expectedBrandArtifacts(t, root)
	if len(artifacts) != want {
		t.Fatalf("renderAll() produced %d artifacts, want %d", len(artifacts), want)
	}
	if got := brandArtifactCount(t, root, artifacts); got != want-54 {
		t.Fatalf("brand artifacts = %d, want %d", got, want-54)
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
	bare := filepath.Join(commands, "bare.alias")
	if err := os.WriteFile(bare, []byte("memory-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderAlias(bare, &target{Agent: "Claude Code"}); err == nil || !strings.Contains(err.Error(), ".md.alias") {
		t.Fatalf("alias without .md.alias suffix should be rejected by name, got %v", err)
	}
	self := filepath.Join(commands, "memory-x.md.alias")
	if err := os.WriteFile(self, []byte("memory-x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderAlias(self, &target{Agent: "Claude Code"}); err == nil {
		t.Fatal("self-alias should be rejected")
	}
}

func TestClaimOutputRejectsDuplicateArtifactPath(t *testing.T) {
	seen := map[string]string{}
	if err := claimOutput(seen, "/out/commands/foo.md", "foo.md.tmpl"); err != nil {
		t.Fatal(err)
	}
	err := claimOutput(seen, "/out/commands/./foo.md", "foo.md.alias")
	if err == nil || !strings.Contains(err.Error(), "foo.md.tmpl") {
		t.Fatalf("expected duplicate-path error naming the first source, got %v", err)
	}
}

// brandArtifactCount counts artifacts under plugins/brands/ so the corpus
// expectation tracks the manifest's brand list without hard-coding it.
func brandArtifactCount(t *testing.T, root string, artifacts []artifact) int {
	t.Helper()
	prefix := filepath.Join(root, filepath.FromSlash(brandOutputPrefix)) + string(filepath.Separator)
	n := 0
	for i := range artifacts {
		if strings.HasPrefix(artifacts[i].Path, prefix) {
			n++
		}
	}
	return n
}

// expectedBrandArtifacts derives each brand's artifact count from the
// manifest and the base plugin's source files.
func expectedBrandArtifacts(t *testing.T, root string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "plugins", "prompt-source", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := decodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	bases := map[string]target{}
	for i := range spec.Targets {
		bases[spec.Targets[i].Name] = spec.Targets[i]
	}
	total := 0
	for _, b := range spec.Brands {
		base := bases[b.Base]
		total += countFiles(t, filepath.Join(root, "plugins", "prompt-source", base.Surface), func(name string) bool {
			return strings.HasSuffix(name, ".tmpl") || strings.HasSuffix(name, ".md.alias")
		})
		for _, name := range copiedFiles {
			total += countFiles(t, filepath.Join(root, base.Output, name), func(string) bool { return true })
		}
		total += 2 // plugin.json and README
	}
	return total
}

func countFiles(t *testing.T, dir string, keep func(string) bool) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && keep(entry.Name()) {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

func TestWriteAllPreservesCopyModeAndPrunesStaleBrandFiles(t *testing.T) {
	root := t.TempDir()
	output := "plugins/brands/acme"
	hooks := filepath.Join(root, output, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(hooks, "removed-upstream.sh")
	if err := os.WriteFile(stale, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooks, "gate.sh")
	// Pre-existing non-executable file: the copy's mode must still win.
	if err := os.WriteFile(hook, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := &target{Output: output}
	arts := []artifact{
		{Path: hook, Content: []byte("#!/bin/sh\n"), Target: target, Copied: true, Mode: 0o755},
		{Path: filepath.Join(root, output, "commands", "memory.md"), Content: []byte("# m\n"), Target: target},
	}
	if err := writeAll(root, arts); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hook)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("copied hook is not executable: %v", info.Mode())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale brand file still present (err=%v)", err)
	}
	if err := checkAll(root, arts); err != nil {
		t.Fatalf("check after write should be clean, got %v", err)
	}
}

func TestBrandArtifactCountMatchesBrandPrefix(t *testing.T) {
	root := t.TempDir()
	arts := []artifact{
		{Path: filepath.Join(root, "plugins", "brands", "acme", "hooks", "a.sh")},
		{Path: filepath.Join(root, "plugins", "claude-code", "commands", "x.md")},
	}
	if got := brandArtifactCount(t, root, arts); got != 1 {
		t.Fatalf("brandArtifactCount = %d, want 1", got)
	}
}

func TestCheckAllReportsStaleCopiedBrandFile(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join("plugins", "brands", "acme")
	dir := filepath.Join(root, output, "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.sh")
	if err := os.WriteFile(keep, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "removed-upstream.sh"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := &target{Output: filepath.ToSlash(output)}
	err := checkAll(root, []artifact{{Path: keep, Content: []byte("ok"), Target: target, Copied: true}})
	if err == nil || !strings.Contains(err.Error(), "hooks/removed-upstream.sh (unexpected)") {
		t.Fatalf("checkAll() error = %v, want the stale copied file reported", err)
	}
}

func TestBrandTargetNamesItself(t *testing.T) {
	base := &target{Name: "claude-memory", Surface: "memory", Harness: "claude", Output: "plugins/claude-code", PluginName: "demarkus-memory", MemoryPluginName: "demarkus-memory", KnowledgePluginName: "demarkus-knowledge"}
	b := &brand{Name: "acme", Base: "claude-memory", Output: "plugins/brands/acme", PluginName: "acme-brain", Description: "d"}
	got := brandTarget(b, base)
	if got.PluginName != "acme-brain" || got.MemoryPluginName != "acme-brain" || got.KnowledgePluginName != "demarkus-knowledge" || got.Output != b.Output {
		t.Fatalf("unexpected brand target: %+v", got)
	}
	b.KnowledgePluginName = "acme-knowledge"
	if brandTarget(b, base).KnowledgePluginName != "acme-knowledge" {
		t.Fatal("brand knowledge_plugin_name should override the base sibling name")
	}
}

func TestValidateBrandsRejectsBadEntries(t *testing.T) {
	base := target{Name: "claude-memory", Surface: "memory", Harness: "claude", Output: "plugins/claude-code", PluginName: "demarkus-memory"}
	pi := target{Name: "pi-memory", Surface: "memory", Harness: "pi", Output: "plugins/pi-memory", PluginName: "demarkus-memory"}
	ok := brand{Name: "acme", Base: "claude-memory", Output: "plugins/brands/acme", PluginName: "acme-brain", Description: "d"}
	cases := map[string]brand{
		"unknown base":    {Name: "x", Base: "nope", Output: "plugins/brands/x", PluginName: "x-brain", Description: "d"},
		"non-claude base": {Name: "x", Base: "pi-memory", Output: "plugins/brands/x", PluginName: "x-brain", Description: "d"},
		"bad plugin name": {Name: "x", Base: "claude-memory", Output: "plugins/brands/x", PluginName: "X Brain", Description: "d"},
		"base name":       {Name: "x", Base: "claude-memory", Output: "plugins/brands/x", PluginName: "demarkus-memory", Description: "d"},
		"output outside":  {Name: "x", Base: "claude-memory", Output: "plugins/x", PluginName: "x-brain", Description: "d"},
		"bad sibling":     {Name: "x", Base: "claude-memory", Output: "plugins/brands/x", PluginName: "x-brain", Description: "d", MemoryPluginName: "bad\nname"},
	}
	if err := validateBrands(&manifest{Targets: []target{base, pi}, Brands: []brand{ok}}); err != nil {
		t.Fatalf("valid brand rejected: %v", err)
	}
	for name, bad := range cases {
		if err := validateBrands(&manifest{Targets: []target{base, pi}, Brands: []brand{bad}}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := validateBrands(&manifest{Targets: []target{base}, Brands: []brand{ok, ok}}); err == nil {
		t.Error("duplicate brand should be rejected")
	}
}

func TestBrandPluginJSONRewritesNameAndDescription(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "plugin.json")
	src := "{\n  \"name\": \"demarkus-memory\",\n  \"version\": \"0.1.0\",\n  \"description\": \"base \\\"quoted\\\" text\",\n  \"hooks\": {}\n}\n"
	if err := os.WriteFile(basePath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := brandPluginJSON(basePath, "demarkus-memory", &brand{PluginName: "acme-brain", Description: "Acme \"brain\""})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"\"name\": \"acme-brain\"", "\"description\": \"Acme \\\"brain\\\"\"", "\"version\": \"0.1.0\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}
