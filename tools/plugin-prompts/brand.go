package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// brand renders a base target under another plugin name: prompts re-rendered
// with the brand's names, hooks/scripts/.mcp.json copied verbatim (server key
// and ~/.demarkus state stay shared), plugin.json and README carrying the brand.
type brand struct {
	Name                string `json:"name"`
	Base                string `json:"base"`
	Output              string `json:"output"`
	PluginName          string `json:"plugin_name"`
	Description         string `json:"description"`
	MemoryPluginName    string `json:"memory_plugin_name"`
	KnowledgePluginName string `json:"knowledge_plugin_name"`
}

const brandOutputPrefix = "plugins/brands/"

var pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// copiedFiles are the base plugin's non-generated files a brand ships as-is.
var copiedFiles = []string{".mcp.json", "hooks", "scripts"}

func validateBrands(spec *manifest) error {
	targets := map[string]*target{}
	for i := range spec.Targets {
		targets[spec.Targets[i].Name] = &spec.Targets[i]
	}
	names := map[string]struct{}{}
	outputs := map[string]struct{}{}
	for i := range spec.Brands {
		b := &spec.Brands[i]
		base, ok := targets[b.Base]
		if !ok {
			return fmt.Errorf("brand %q: unknown base target %q", b.Name, b.Base)
		}
		if base.Harness != "claude" {
			return fmt.Errorf("brand %q: only claude targets can be branded (base %q is %s)", b.Name, b.Base, base.Harness)
		}
		if b.Name == "" || b.Description == "" || !pluginNameRE.MatchString(b.PluginName) {
			return fmt.Errorf("brand %q: name, description, and a lowercase plugin_name are required", b.Name)
		}
		for _, sibling := range []string{b.MemoryPluginName, b.KnowledgePluginName} {
			if sibling != "" && !pluginNameRE.MatchString(sibling) {
				return fmt.Errorf("brand %q: sibling plugin name %q must be lowercase letters, digits, and hyphens", b.Name, sibling)
			}
		}
		if b.PluginName == base.PluginName {
			return fmt.Errorf("brand %q: plugin_name %q is the base plugin's own name", b.Name, b.PluginName)
		}
		if !strings.HasPrefix(b.Output, brandOutputPrefix) || hasParentTraversal(b.Output) {
			return fmt.Errorf("brand %q: output must live under %s", b.Name, brandOutputPrefix)
		}
		if _, dup := names[b.Name]; dup {
			return fmt.Errorf("brand %q: duplicate name", b.Name)
		}
		if _, dup := outputs[b.Output]; dup {
			return fmt.Errorf("brand %q: duplicate output %s", b.Name, b.Output)
		}
		names[b.Name] = struct{}{}
		outputs[b.Output] = struct{}{}
	}
	return nil
}

// brandTarget derives the render target for a brand from its base.
func brandTarget(b *brand, base *target) *target {
	t := *base
	t.Name = b.Name
	t.Output = b.Output
	t.PluginName = b.PluginName
	t.MemoryPluginName = b.MemoryPluginName
	t.KnowledgePluginName = b.KnowledgePluginName
	if t.MemoryPluginName == "" {
		t.MemoryPluginName = base.MemoryPluginName
	}
	if t.KnowledgePluginName == "" {
		t.KnowledgePluginName = base.KnowledgePluginName
	}
	// The branded surface refers to itself by its own name.
	if base.Surface == "memory" {
		t.MemoryPluginName = b.PluginName
	} else {
		t.KnowledgePluginName = b.PluginName
	}
	return &t
}

// brandArtifacts returns the copied and rewritten files for one brand.
func brandArtifacts(root string, b *brand, base, t *target) ([]artifact, error) {
	var out []artifact
	baseDir := filepath.Join(root, base.Output)
	outDir := filepath.Join(root, b.Output)
	for _, name := range copiedFiles {
		err := filepath.WalkDir(filepath.Join(baseDir, name), func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(baseDir, path)
			if err != nil {
				return err
			}
			out = append(out, artifact{Path: filepath.Join(outDir, rel), Content: content, Target: t, Copied: true})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("brand %s: copy %s: %w", b.Name, name, err)
		}
	}
	pluginJSON, err := brandPluginJSON(filepath.Join(baseDir, ".claude-plugin", "plugin.json"), base.PluginName, b)
	if err != nil {
		return nil, fmt.Errorf("brand %s: %w", b.Name, err)
	}
	out = append(out,
		artifact{Path: filepath.Join(outDir, ".claude-plugin", "plugin.json"), Content: pluginJSON, Target: t, Copied: true},
		artifact{Path: filepath.Join(outDir, "README.md"), Content: []byte(brandReadme(b, base)), Target: t, Copied: true},
	)
	return out, nil
}

var (
	nameFieldRE = regexp.MustCompile(`(?m)^ {2}"name": "(?:[^"\\]|\\.)*",$`)
	descFieldRE = regexp.MustCompile(`(?m)^ {2}"description": "(?:[^"\\]|\\.)*",$`)
)

// brandPluginJSON rewrites the base plugin.json's top-level name and
// description in place, keeping every other field (hooks, version) verbatim.
func brandPluginJSON(basePath, baseName string, b *brand) ([]byte, error) {
	raw, err := os.ReadFile(basePath)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if !nameFieldRE.MatchString(text) || !descFieldRE.MatchString(text) {
		return nil, fmt.Errorf("%s: expected top-level name and description lines", basePath)
	}
	name, _ := json.Marshal(b.PluginName)
	desc, _ := json.Marshal(b.Description)
	text = nameFieldRE.ReplaceAllLiteralString(text, `  "name": `+string(name)+",")
	text = descFieldRE.ReplaceAllLiteralString(text, `  "description": `+string(desc)+",")
	if !json.Valid([]byte(text)) || !strings.Contains(text, string(name)) || strings.Contains(text, `"name": "`+baseName+`"`) {
		return nil, fmt.Errorf("%s: branded plugin.json did not rewrite cleanly", basePath)
	}
	return []byte(text), nil
}

func brandReadme(b *brand, base *target) string {
	return fmt.Sprintf(`# %s

%s

This plugin is generated from the %s plugin in the demarkus repository
(source: %s). Prompts, hooks, and scripts are identical apart from the plugin
name; it shares the demarkus-plugin binary, the "demarkus-memory" MCP server key,
and the ~/.demarkus state, so install it instead of, not alongside, %s.

Regenerate after editing the templates or the base plugin:

    cd tools && go run ./plugin-prompts write
`, b.PluginName, b.Description, base.PluginName, base.Output, base.PluginName)
}
