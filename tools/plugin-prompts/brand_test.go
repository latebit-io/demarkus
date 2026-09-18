package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeBrandsFileIsStrict(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown field", raw: `{"brands": [], "targets": []}`, want: "unknown field"},
		{name: "trailing value", raw: `{"brands": []} {}`, want: "trailing"},
		{name: "array root", raw: `[]`, want: "cannot unmarshal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeBrandsFile([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadBrandsFileMissingErrorsEmptyLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brands.json")
	if _, err := loadBrandsFile(path); err == nil || !strings.Contains(err.Error(), "read brands file") {
		t.Fatalf("missing file err = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"brands": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	brands, err := loadBrandsFile(path)
	if err != nil || len(brands) != 0 {
		t.Fatalf("empty list = %v, %v", brands, err)
	}
}

func TestRenderAllAddsBrandsFromFile(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "brands.json")
	spec := `{"brands": [{"name": "acme-brain", "base": "claude-memory", "output": "plugins/brands/acme-brain",
		"plugin_name": "acme-brain", "knowledge_plugin_name": "acme-knowledge", "description": "Acme Brain."}]}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts, err := renderAll(root, path)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := renderAll(root, "")
	if err != nil {
		t.Fatal(err)
	}
	branded := brandArtifactCount(t, root, artifacts) - brandArtifactCount(t, root, baseline)
	if branded <= 0 || len(artifacts)-len(baseline) != branded {
		t.Fatalf("brand added %d artifacts, %d under plugins/brands/", len(artifacts)-len(baseline), branded)
	}
	outDir := filepath.Join(root, "plugins", "brands", "acme-brain")
	seen := map[string]bool{}
	for i := range artifacts {
		a := &artifacts[i]
		if !strings.HasPrefix(a.Path, outDir+string(filepath.Separator)) {
			continue
		}
		rel := filepath.ToSlash(strings.TrimPrefix(a.Path, outDir+string(filepath.Separator)))
		seen[rel] = true
		if a.Target.PluginName != "acme-brain" || a.Target.KnowledgePluginName != "acme-knowledge" {
			t.Fatalf("%s: target names = %q %q", rel, a.Target.PluginName, a.Target.KnowledgePluginName)
		}
		if strings.Contains(string(a.Content), "/demarkus-memory:") {
			t.Fatalf("%s: still names the base plugin", rel)
		}
	}
	for _, want := range []string{".claude-plugin/plugin.json", "README.md", ".mcp.json", "context/session-guidance.md"} {
		if !seen[want] {
			t.Fatalf("brand output lacks %s", want)
		}
	}
}

func TestRenderAllBrandsCursorBase(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "brands.json")
	spec := `{"brands": [{"name": "acme-cursor", "base": "cursor-knowledge", "output": "plugins/brands/acme-cursor",
		"plugin_name": "acme-knowledge", "memory_plugin_name": "acme-brain", "description": "Acme Knowledge."}]}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts, err := renderAll(root, path)
	if err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "plugins", "brands", "acme-cursor") + string(filepath.Separator)
	seen := map[string]bool{}
	for i := range artifacts {
		if rel, ok := strings.CutPrefix(artifacts[i].Path, outDir); ok {
			seen[filepath.ToSlash(rel)] = true
		}
	}
	for _, want := range []string{".cursor-plugin/plugin.json", "hooks/gate.sh", "scripts/bootstrap.sh", "README.md"} {
		if !seen[want] {
			t.Fatalf("cursor brand lacks %s", want)
		}
	}
	if seen["mcp.json"] || seen[".claude-plugin/plugin.json"] {
		t.Fatal("cursor knowledge brand copied a memory or claude file")
	}
}

// writeBaseFixture lays out a minimal base plugin under root/output.
func writeBaseFixture(t *testing.T, root, output string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, output, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBrandArtifactsRequiresEveryCopiedRoot(t *testing.T) {
	const manifest = "{\n  \"name\": \"demarkus-memory\",\n  \"description\": \"base\",\n  \"version\": \"1\"\n}\n"
	tests := []struct {
		name    string
		surface string
		files   map[string]string
		wantErr string
	}{
		{name: "knowledge without mcp config", surface: "knowledge",
			files: map[string]string{"hooks/a.sh": "", "scripts/b.sh": "", ".claude-plugin/plugin.json": manifest}},
		{name: "memory without mcp config", surface: "memory",
			files:   map[string]string{"hooks/a.sh": "", "scripts/b.sh": "", ".claude-plugin/plugin.json": manifest},
			wantErr: "copy .mcp.json"},
		{name: "memory without hooks", surface: "memory",
			files:   map[string]string{"scripts/b.sh": "", ".mcp.json": "{}", ".claude-plugin/plugin.json": manifest},
			wantErr: "copy hooks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			base := &target{Name: "base", Surface: tt.surface, Harness: "claude", Output: "plugins/base", PluginName: "demarkus-memory"}
			writeBaseFixture(t, root, base.Output, tt.files)
			b := &brand{Name: "acme", Base: "base", Output: "plugins/brands/acme", PluginName: "acme", Description: "Acme."}
			_, err := brandArtifacts(root, b, base, brandTarget(b, base))
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateBrandsPluginNameUniquePerHarness(t *testing.T) {
	targets := []target{
		{Name: "claude-memory", Surface: "memory", Harness: "claude", PluginName: "demarkus-memory"},
		{Name: "claude-knowledge", Surface: "knowledge", Harness: "claude", PluginName: "demarkus-knowledge"},
		{Name: "cursor-memory", Surface: "memory", Harness: "cursor", PluginName: "demarkus-memory"},
	}
	entry := func(name, base string) brand {
		return brand{Name: name, Base: base, Output: "plugins/brands/" + name, PluginName: "acme", Description: "Acme."}
	}
	across := &manifest{Targets: targets, Brands: []brand{entry("acme", "claude-memory"), entry("acme-cursor", "cursor-memory")}}
	if err := validateBrands(across); err != nil {
		t.Fatalf("same plugin_name across harnesses: %v", err)
	}
	within := &manifest{Targets: targets, Brands: []brand{entry("acme", "claude-memory"), entry("acme-k", "claude-knowledge")}}
	if err := validateBrands(within); err == nil || !strings.Contains(err.Error(), `duplicate plugin_name "acme" on harness claude`) {
		t.Fatalf("same plugin_name within a harness: err = %v", err)
	}
}

func TestValidateBrandsRejectsUnbrandableHarness(t *testing.T) {
	spec := &manifest{
		Targets: []target{{Name: "pi-memory", Surface: "memory", Harness: "pi", PluginName: "demarkus-pi-memory"}},
		Brands:  []brand{{Name: "acme", Base: "pi-memory", Output: "plugins/brands/acme", PluginName: "acme", Description: "Acme."}},
	}
	if err := validateBrands(spec); err == nil || !strings.Contains(err.Error(), "only claude and cursor") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderAllRejectsBrandsFileDuplicatingName(t *testing.T) {
	root, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "brands.json")
	entry := `{"name": "acme", "base": "claude-memory", "output": "plugins/brands/acme", "plugin_name": "acme", "description": "Acme."}`
	if err := os.WriteFile(path, []byte(`{"brands": [`+entry+`, `+entry+`]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := renderAll(root, path); err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreDefaultsAndBrandNoun(t *testing.T) {
	base := &target{Name: "b", Surface: "memory"}
	applyStoreDefaults(base)
	if base.Store != "soul" || base.Stores != "souls" || base.StoreTitle != "Soul" {
		t.Fatalf("defaults = %q %q %q", base.Store, base.Stores, base.StoreTitle)
	}
	multi := &target{Store: "âme"}
	applyStoreDefaults(multi)
	if multi.StoreTitle != "Âme" {
		t.Fatalf("multi-byte title = %q", multi.StoreTitle)
	}
	bt := brandTarget(&brand{Name: "x", Base: "b", PluginName: "x", Stores: "vaults"}, base)
	if bt.Store != "soul" || bt.Stores != "vaults" {
		t.Fatalf("plural-only override = %q %q", bt.Store, bt.Stores)
	}
	bt = brandTarget(&brand{Name: "y", Base: "b", PluginName: "y", Store: "vault"}, base)
	if bt.Store != "vault" || bt.Stores != "vaults" || bt.StoreTitle != "Vault" {
		t.Fatalf("singular-only override = %q %q %q", bt.Store, bt.Stores, bt.StoreTitle)
	}
}
