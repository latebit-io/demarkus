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

func TestLoadBrandsFileRejectsEmptyAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brands.json")
	if _, err := loadBrandsFile(path); err == nil || !strings.Contains(err.Error(), "read brands file") {
		t.Fatalf("missing file err = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"brands": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrandsFile(path); err == nil || !strings.Contains(err.Error(), "no brands") {
		t.Fatalf("empty file err = %v", err)
	}
}

// A brands file renders like a manifest brand: prompts under the brand name,
// base files copied, plugin.json and README rewritten, all under plugins/brands/.
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
