package main

import (
	"encoding/json"
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
		{name: "null brands", raw: `{"brands": null}`, want: "must be an array"},
		{name: "missing brands", raw: `{}`, want: "must be an array"},
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

func TestBrandMCPConfigRenamesServerAndPassesName(t *testing.T) {
	base := []byte("{\n  \"mcpServers\": {\n    \"demarkus-memory\": {\n      \"command\": \"${HOME}/.demarkus/bin/demarkus-plugin\",\n      \"args\": [\"mcp-serve\"]\n    }\n  }\n}\n")
	out, err := brandMCPConfig(base, "memory")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	entry, ok := doc.Servers["memory"]
	if !ok || len(doc.Servers) != 1 {
		t.Fatalf("servers = %v", doc.Servers)
	}
	if entry.Command != "${HOME}/.demarkus/bin/demarkus-plugin" || strings.Join(entry.Args, " ") != "mcp-serve --name memory" {
		t.Fatalf("entry = %+v", entry)
	}
	for name, raw := range map[string]string{
		"other server":  `{"mcpServers": {"other": {}}}`,
		"null entry":    `{"mcpServers": {"demarkus-memory": null}}`,
		"args missing":  `{"mcpServers": {"demarkus-memory": {"command": "x"}}}`,
		"args not list": `{"mcpServers": {"demarkus-memory": {"command": "x", "args": "mcp-serve"}}}`,
	} {
		if _, err := brandMCPConfig([]byte(raw), "memory"); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestValidateBrandsMCPServerKey(t *testing.T) {
	targets := []target{
		{Name: "claude-memory", Surface: "memory", Harness: "claude", PluginName: "demarkus-memory"},
		{Name: "claude-knowledge", Surface: "knowledge", Harness: "claude", PluginName: "demarkus-knowledge"},
	}
	entry := func(base, key string) brand {
		return brand{Name: "acme", Base: base, Output: "plugins/brands/acme", PluginName: "acme", Description: "Acme.", MCPServerKey: key}
	}
	tests := []struct {
		name string
		b    brand
		want string
	}{
		{name: "memory base", b: entry("claude-memory", "memory")},
		{name: "knowledge base", b: entry("claude-knowledge", "memory"), want: "memory bases only"},
		{name: "default key", b: entry("claude-memory", "demarkus-memory"), want: "other than"},
		{name: "bad key", b: entry("claude-memory", "Memory!"), want: "lowercase"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBrands(&manifest{Targets: targets, Brands: []brand{tt.b}})
			if tt.want == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBrandArtifactsRewriteMCPConfig(t *testing.T) {
	root := t.TempDir()
	base := &target{Name: "base", Surface: "memory", Harness: "claude", Output: "plugins/base", PluginName: "demarkus-memory"}
	writeBaseFixture(t, root, base.Output, map[string]string{
		"hooks/a.sh": "", "scripts/b.sh": "",
		".mcp.json":                  `{"mcpServers": {"demarkus-memory": {"command": "x", "args": ["mcp-serve"]}}}`,
		".claude-plugin/plugin.json": "{\n  \"name\": \"demarkus-memory\",\n  \"description\": \"base\",\n  \"version\": \"1\"\n}\n",
	})
	b := &brand{Name: "acme", Base: "base", Output: "plugins/brands/acme", PluginName: "acme", Description: "Acme.", MCPServerKey: "memory"}
	arts, err := brandArtifacts(root, b, base, brandTarget(b, base))
	if err != nil {
		t.Fatal(err)
	}
	for i := range arts {
		if filepath.Base(arts[i].Path) == ".mcp.json" {
			if text := string(arts[i].Content); strings.Contains(text, "demarkus-memory") {
				t.Fatalf(".mcp.json still names the base server: %s", text)
			}
			return
		}
	}
	t.Fatal(".mcp.json not produced")
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

func TestWriteAllRemovesDirectoryOfDroppedBrand(t *testing.T) {
	root := t.TempDir()
	writeBaseFixture(t, root, "plugins/brands", map[string]string{"README.md": "guide", "old/hooks/x.sh": "", "old/.claude-plugin/plugin.json": "{}"})
	old := filepath.Join(root, "plugins", "brands", "old")
	err := checkAll(root, nil)
	if err == nil || !strings.Contains(err.Error(), "plugins/brands/old/hooks/x.sh (unexpected)") {
		t.Fatalf("check before prune: %v", err)
	}
	if err := writeAll(root, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("dropped brand directory still present (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugins", "brands", "README.md")); err != nil {
		t.Fatalf("hand-kept README removed: %v", err)
	}
	if err := checkAll(root, nil); err != nil {
		t.Fatalf("check after prune: %v", err)
	}
}

// copyTree copies src into dst, keeping file modes.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The downstream lifecycle: write a brand, then write with the brand removed
// from the file; the old output must go, and check must flag it in between.
func TestWriteWithEmptyBrandsFileRemovesEarlierBrand(t *testing.T) {
	repo, err := findRoot()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, dir := range []string{"plugins/prompt-source", "plugins/claude-code/hooks", "plugins/claude-code/scripts", "plugins/claude-code/.claude-plugin"} {
		copyTree(t, filepath.Join(repo, filepath.FromSlash(dir)), filepath.Join(root, filepath.FromSlash(dir)))
	}
	for _, file := range []string{"plugins/claude-code/.mcp.json", "plugins/brands/README.md"} {
		copyTree(t, filepath.Join(repo, filepath.FromSlash(file)), filepath.Join(root, filepath.FromSlash(file)))
	}
	brands := filepath.Join(t.TempDir(), "brands.json")
	entry := `{"brands": [{"name": "acme", "base": "claude-memory", "output": "plugins/brands/acme", "plugin_name": "acme", "description": "Acme."}]}`
	if err := os.WriteFile(brands, []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	artifacts, err := renderAll(root, brands)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAll(root, artifacts); err != nil {
		t.Fatal(err)
	}
	acme := filepath.Join(root, "plugins", "brands", "acme")
	if _, err := os.Stat(filepath.Join(acme, "hooks")); err != nil {
		t.Fatalf("brand not written: %v", err)
	}
	if err := os.WriteFile(brands, []byte(`{"brands": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if artifacts, err = renderAll(root, brands); err != nil {
		t.Fatal(err)
	}
	if err := checkAll(root, artifacts); err == nil || !strings.Contains(err.Error(), "plugins/brands/acme/") {
		t.Fatalf("check with the brand removed: %v", err)
	}
	if err := writeAll(root, artifacts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acme); !os.IsNotExist(err) {
		t.Fatalf("removed brand output still present (err=%v)", err)
	}
	if err := checkAll(root, artifacts); err != nil {
		t.Fatalf("check after prune: %v", err)
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

func TestDecodeBrandsFileRejectsUnknownAuthorField(t *testing.T) {
	raw := `{"brands": [{"name": "acme", "author": {"name": "Acme", "email": "x@acme.test"}}]}`
	if _, err := decodeBrandsFile([]byte(raw)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateBrandIdentity(t *testing.T) {
	tests := []struct {
		name string
		b    brand
		want string
	}{
		{name: "unset"},
		{name: "full", b: brand{Author: &brandAuthor{Name: "Acme", URL: "https://acme.test"}, Homepage: "https://acme.test/plugins", Repository: "https://github.com/acme/plugins"}},
		{name: "author without name", b: brand{Author: &brandAuthor{URL: "https://acme.test"}}, want: "author.name is required"},
		{name: "author url relative", b: brand{Author: &brandAuthor{Name: "Acme", URL: "acme.test"}}, want: "author.url"},
		{name: "homepage scheme", b: brand{Homepage: "ftp://acme.test"}, want: "homepage"},
		{name: "repository no host", b: brand{Repository: "https://"}, want: "repository"},
		{name: "homepage port only", b: brand{Homepage: "https://:443"}, want: "homepage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBrandIdentity(&tt.b)
			if tt.want == "" && err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

const identityManifest = `{
  "name": "demarkus-memory",
  "version": "0.1.0",
  "description": "base",
  "author": {
    "name": "latebit",
    "url": "https://github.com/latebit-io"
  },
  "homepage": "https://github.com/latebit-io/demarkus",
  "license": "MIT",
  "hooks": {}
}
`

func writeManifest(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBrandPluginJSONIdentityFields(t *testing.T) {
	b := &brand{
		PluginName: "acme-brain", Description: "Acme.",
		Author:   &brandAuthor{Name: "Acme"},
		Homepage: "https://acme.test/plugins", Repository: "https://github.com/acme/plugins",
	}
	got, err := brandPluginJSON(writeManifest(t, identityManifest), b)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "name": "acme-brain",
  "version": "0.1.0",
  "description": "Acme.",
  "author": {
    "name": "Acme"
  },
  "homepage": "https://acme.test/plugins",
  "repository": "https://github.com/acme/plugins",
  "license": "MIT",
  "hooks": {}
}
`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBrandPluginJSONKeepsBaseIdentityWhenUnset(t *testing.T) {
	got, err := brandPluginJSON(writeManifest(t, identityManifest), &brand{PluginName: "acme-brain", Description: "Acme."})
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`"name": "latebit"`, `"homepage": "https://github.com/latebit-io/demarkus"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("base identity dropped, missing %s in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "repository") {
		t.Fatalf("repository invented:\n%s", text)
	}
}

func TestBrandPluginJSONRejectsUnsupportedLayout(t *testing.T) {
	tests := map[string]string{
		"inline author":  "{\n  \"name\": \"demarkus-memory\",\n  \"description\": \"base\",\n  \"author\": {\"name\": \"latebit\"},\n  \"hooks\": {}\n}\n",
		"repeated field": "{\n  \"name\": \"demarkus-memory\",\n  \"description\": \"base\",\n  \"homepage\": \"https://a.test\",\n  \"homepage\": \"https://b.test\",\n  \"hooks\": {}\n}\n",
	}
	b := &brand{PluginName: "acme-brain", Description: "Acme.", Author: &brandAuthor{Name: "Acme"}, Homepage: "https://acme.test"}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := brandPluginJSON(writeManifest(t, text), b); err == nil || !strings.Contains(err.Error(), "not one single- or block-formatted entry") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestBrandReadmeMentionsMCPKeyForMemoryOnly(t *testing.T) {
	b := &brand{PluginName: "acme-brain", Description: "Acme."}
	memory := &target{Surface: "memory", PluginName: "demarkus-memory", Output: "plugins/claude-code"}
	if text := brandReadme(b, memory, brandTarget(b, memory)); !strings.Contains(text, `MCP server key "demarkus-memory"`) {
		t.Fatalf("memory README lacks the server key:\n%s", text)
	}
	knowledge := &target{Surface: "knowledge", PluginName: "demarkus-knowledge", Output: "plugins/claude-code-knowledge"}
	text := brandReadme(b, knowledge, brandTarget(b, knowledge))
	if strings.Contains(text, "MCP server key") || strings.Contains(text, "local memory") {
		t.Fatalf("knowledge README claims a local memory:\n%s", text)
	}
	if strings.Contains(text, "cd tools") {
		t.Fatalf("README regen line assumes the demarkus checkout is the cwd:\n%s", text)
	}
}

func TestBrandPluginJSONInsertsAfterNearestPredecessor(t *testing.T) {
	base := "{\n  \"name\": \"demarkus-memory\",\n  \"description\": \"base\",\n  \"homepage\": \"https://a.test\",\n  \"license\": \"MIT\"\n}\n"
	b := &brand{PluginName: "acme-brain", Description: "Acme.", Author: &brandAuthor{Name: "Acme"}, Repository: "https://github.com/acme/plugins"}
	got, err := brandPluginJSON(writeManifest(t, base), b)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"name\": \"acme-brain\",\n  \"description\": \"Acme.\",\n  \"author\": {\n    \"name\": \"Acme\"\n  },\n  \"homepage\": \"https://a.test\",\n  \"repository\": \"https://github.com/acme/plugins\",\n  \"license\": \"MIT\"\n}\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
