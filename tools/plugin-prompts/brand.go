package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	Store               string `json:"store_noun"`
	Stores              string `json:"store_noun_plural"`
	// MCPServerKey renames the memory MCP server (default "demarkus-memory"), so
	// tools read mcp__plugin_<brand>_<key>__mark_*; mcp-serve --name tells the gates.
	MCPServerKey string `json:"mcp_server_key"`
	// Manifest identity; each replaces the base plugin.json field when set and
	// keeps the base's when not.
	Author     *brandAuthor `json:"author"`
	Homepage   string       `json:"homepage"`
	Repository string       `json:"repository"`
}

// brandAuthor is the plugin manifest's author object.
type brandAuthor struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

const defaultMCPServerKey = "demarkus-memory"

const brandOutputPrefix = "plugins/brands/"

// brandsFile is the --brands document: brands kept outside the manifest so a
// downstream repository can render them against an unmodified checkout.
type brandsFile struct {
	Brands []brand `json:"brands"`
}

func loadBrandsFile(path string) ([]brand, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read brands file: %w", err)
	}
	spec, err := decodeBrandsFile(raw)
	if err != nil {
		return nil, fmt.Errorf("parse brands file %s: %w", path, err)
	}
	// An empty list is valid: a downstream regen with every brand removed
	// still needs a successful run to prune its outputs.
	return spec.Brands, nil
}

func decodeBrandsFile(raw []byte) (brandsFile, error) {
	var spec brandsFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return brandsFile{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return brandsFile{}, errors.New("unexpected trailing JSON value")
		}
		return brandsFile{}, err
	}
	// A missing or null list would read as "remove every brand" downstream.
	if spec.Brands == nil {
		return brandsFile{}, errors.New(`"brands" must be an array`)
	}
	return spec, nil
}

var pluginNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// harnessLayout names a base plugin's non-generated files: the manifest is
// rewritten with the brand's name, the rest is copied as-is. Harnesses absent
// here (pi, OpenCode) carry their names in source and cannot be branded.
type harnessLayout struct {
	Manifest string   // plugin manifest with top-level name and description
	MCP      string   // MCP server config; memory bases only, knowledge registers MCP at join
	Copied   []string // directories shipped verbatim
}

var brandLayouts = map[string]harnessLayout{
	"claude": {Manifest: ".claude-plugin/plugin.json", MCP: ".mcp.json", Copied: []string{"hooks", "scripts"}},
	"cursor": {Manifest: ".cursor-plugin/plugin.json", MCP: "mcp.json", Copied: []string{"hooks", "scripts"}},
}

// brandCopiedRoots lists the base files a brand copies; each must exist.
func brandCopiedRoots(base *target) []string {
	layout := brandLayouts[base.Harness]
	roots := append([]string(nil), layout.Copied...)
	if base.Surface == "memory" {
		roots = append(roots, layout.MCP)
	}
	return roots
}

func validateBrands(spec *manifest) error {
	targets := map[string]*target{}
	for i := range spec.Targets {
		targets[spec.Targets[i].Name] = &spec.Targets[i]
	}
	names := map[string]struct{}{}
	outputs := map[string]struct{}{}
	// plugin_name is unique per harness; harnesses have separate marketplaces.
	pluginNames := map[string]struct{}{}
	for i := range spec.Brands {
		b := &spec.Brands[i]
		base, ok := targets[b.Base]
		if !ok {
			return fmt.Errorf("brand %q: unknown base target %q", b.Name, b.Base)
		}
		if _, ok := brandLayouts[base.Harness]; !ok {
			return fmt.Errorf("brand %q: only claude and cursor targets can be branded (base %q is %s)", b.Name, b.Base, base.Harness)
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
		if b.MCPServerKey != "" {
			if base.Surface != "memory" {
				return fmt.Errorf("brand %q: mcp_server_key applies to memory bases only", b.Name)
			}
			if !pluginNameRE.MatchString(b.MCPServerKey) || b.MCPServerKey == defaultMCPServerKey {
				return fmt.Errorf("brand %q: mcp_server_key must be lowercase letters, digits, and hyphens, other than %q", b.Name, defaultMCPServerKey)
			}
		}
		if err := validateBrandIdentity(b); err != nil {
			return fmt.Errorf("brand %q: %w", b.Name, err)
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
		pluginKey := base.Harness + "/" + b.PluginName
		if _, dup := pluginNames[pluginKey]; dup {
			return fmt.Errorf("brand %q: duplicate plugin_name %q on harness %s", b.Name, b.PluginName, base.Harness)
		}
		names[b.Name] = struct{}{}
		outputs[b.Output] = struct{}{}
		pluginNames[pluginKey] = struct{}{}
	}
	return nil
}

// validateBrandIdentity checks the optional manifest identity fields.
func validateBrandIdentity(b *brand) error {
	if b.Author != nil {
		if b.Author.Name == "" {
			return errors.New("author.name is required when author is set")
		}
		if err := validateWebURL("author.url", b.Author.URL); err != nil {
			return err
		}
	}
	if err := validateWebURL("homepage", b.Homepage); err != nil {
		return err
	}
	return validateWebURL("repository", b.Repository)
}

// validateWebURL accepts an empty value or an absolute http(s) URL.
func validateWebURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%s %q must be an absolute http(s) URL", field, raw)
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
	// A singular override re-derives the plural; a plural-only override keeps the base singular.
	switch {
	case b.Store != "":
		t.Store, t.Stores = b.Store, b.Stores
	case b.Stores != "":
		t.Stores = b.Stores
	}
	if b.MCPServerKey != "" {
		t.MCPServerKey = b.MCPServerKey
	}
	applyStoreDefaults(&t)
	return &t
}

// brandArtifacts returns the copied and rewritten files for one brand.
func brandArtifacts(root string, b *brand, base, t *target) ([]artifact, error) {
	var out []artifact
	baseDir := filepath.Join(root, base.Output)
	outDir := filepath.Join(root, b.Output)
	for _, name := range brandCopiedRoots(base) {
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
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(baseDir, path)
			if err != nil {
				return err
			}
			out = append(out, artifact{Path: filepath.Join(outDir, rel), Content: content, Target: t, Copied: true, Mode: info.Mode().Perm()})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("brand %s: copy %s: %w", b.Name, name, err)
		}
	}
	layout := brandLayouts[base.Harness]
	if t.MCPServerKey != defaultMCPServerKey {
		mcpPath := filepath.Join(outDir, filepath.FromSlash(layout.MCP))
		i := slices.IndexFunc(out, func(a artifact) bool { return a.Path == mcpPath })
		if i < 0 {
			return nil, fmt.Errorf("brand %s: %s not copied from the base", b.Name, layout.MCP)
		}
		content, err := brandMCPConfig(out[i].Content, t.MCPServerKey)
		if err != nil {
			return nil, fmt.Errorf("brand %s: %s: %w", b.Name, layout.MCP, err)
		}
		out[i].Content = content
	}
	manifest := filepath.FromSlash(layout.Manifest)
	pluginJSON, err := brandPluginJSON(filepath.Join(baseDir, manifest), b)
	if err != nil {
		return nil, fmt.Errorf("brand %s: %w", b.Name, err)
	}
	out = append(out,
		artifact{Path: filepath.Join(outDir, manifest), Content: pluginJSON, Target: t, Copied: true},
		artifact{Path: filepath.Join(outDir, "README.md"), Content: []byte(brandReadme(b, base, t)), Target: t, Copied: true},
	)
	return out, nil
}

// brandMCPConfig renames the base MCP server entry to key and appends
// `--name <key>` so mcp-serve records the alias the gates resolve.
func brandMCPConfig(raw []byte, key string) ([]byte, error) {
	var doc struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	entry, ok := doc.Servers[defaultMCPServerKey]
	if !ok || len(doc.Servers) != 1 {
		return nil, fmt.Errorf("expected exactly one MCP server named %q", defaultMCPServerKey)
	}
	if entry == nil {
		return nil, fmt.Errorf("MCP server %q must be an object", defaultMCPServerKey)
	}
	args, ok := entry["args"].([]any)
	if !ok {
		return nil, fmt.Errorf("MCP server %q must have an args array", defaultMCPServerKey)
	}
	entry["args"] = append(args, "--name", key)
	out, err := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{key: entry}}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// manifestField is one rewritable top-level entry of plugin.json. line matches
// the whole entry, trailing comma and newline included, so a replacement or an
// insertion keeps the hand-formatted file intact; key finds any spelling of it.
type manifestField struct {
	name string
	key  *regexp.Regexp
	line *regexp.Regexp
}

func scalarField(name string) manifestField {
	return manifestField{
		name: name,
		key:  regexp.MustCompile(`(?m)^ {2}"` + name + `":`),
		line: regexp.MustCompile(`(?m)^ {2}"` + name + `": "(?:[^"\\]|\\.)*",\n`),
	}
}

func objectField(name string) manifestField {
	return manifestField{
		name: name,
		key:  regexp.MustCompile(`(?m)^ {2}"` + name + `":`),
		line: regexp.MustCompile(`(?m)^ {2}"` + name + `": \{\n(?: {4}.*\n)* {2}\},\n`),
	}
}

var (
	nameField        = scalarField("name")
	descriptionField = scalarField("description")
	// Identity fields: a brand value replaces the base entry or, when the base
	// lacks it, is inserted after the nearest present predecessor.
	authorField     = objectField("author")
	homepageField   = scalarField("homepage")
	repositoryField = scalarField("repository")
)

// render returns the entry line for value, indented as a top-level field.
func (f manifestField) render(value any) (string, error) {
	encoded, err := json.MarshalIndent(value, "  ", "  ")
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", f.name, err)
	}
	return `  "` + f.name + `": ` + string(encoded) + ",\n", nil
}

// replace rewrites the entry for f in text with value or, when the base has
// none, inserts it after the last present anchor (canonical predecessors).
func (f manifestField) replace(text string, value any, anchors []manifestField) (string, error) {
	rendered, err := f.render(value)
	if err != nil {
		return "", err
	}
	switch keys, lines := len(f.key.FindAllStringIndex(text, -1)), len(f.line.FindAllStringIndex(text, -1)); {
	case keys == 1 && lines == 1:
		return f.line.ReplaceAllLiteralString(text, rendered), nil
	case keys == 0:
		at := -1
		for _, anchor := range anchors {
			if loc := anchor.line.FindStringIndex(text); loc != nil {
				at = loc[1]
			}
		}
		if at < 0 {
			return "", fmt.Errorf("no entry to insert %s after", f.name)
		}
		return text[:at] + rendered + text[at:], nil
	default:
		return "", fmt.Errorf("top-level %s is not one single- or block-formatted entry", f.name)
	}
}

// brandPluginJSON rewrites the base plugin.json in place: name and description
// always, author/homepage/repository when the brand sets them; every other
// field (hooks, version, license) stays verbatim.
func brandPluginJSON(basePath string, b *brand) ([]byte, error) {
	raw, err := os.ReadFile(basePath)
	if err != nil {
		return nil, err
	}
	text := string(raw)
	if !nameField.line.MatchString(text) || !descriptionField.line.MatchString(text) {
		return nil, fmt.Errorf("%s: expected top-level name and description lines", basePath)
	}
	fields := []manifestField{nameField, descriptionField, authorField, homepageField, repositoryField}
	values := []struct {
		value any
		set   bool
	}{
		{b.PluginName, true},
		{b.Description, true},
		{b.Author, b.Author != nil},
		{b.Homepage, b.Homepage != ""},
		{b.Repository, b.Repository != ""},
	}
	for i, v := range values {
		if !v.set {
			continue
		}
		if text, err = fields[i].replace(text, v.value, fields[:i]); err != nil {
			return nil, fmt.Errorf("%s: %w", basePath, err)
		}
	}
	if err := checkBrandedManifest([]byte(text), b); err != nil {
		return nil, fmt.Errorf("%s: branded plugin.json did not rewrite cleanly: %w", basePath, err)
	}
	return []byte(text), nil
}

// checkBrandedManifest decodes the rewritten manifest and confirms every
// field the brand sets reads back as its value.
func checkBrandedManifest(raw []byte, b *brand) error {
	var doc struct {
		Name        string       `json:"name"`
		Description string       `json:"description"`
		Author      *brandAuthor `json:"author"`
		Homepage    string       `json:"homepage"`
		Repository  string       `json:"repository"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	switch {
	case doc.Name != b.PluginName:
		return fmt.Errorf("name is %q", doc.Name)
	case doc.Description != b.Description:
		return errors.New("description differs")
	case b.Author != nil && (doc.Author == nil || *doc.Author != *b.Author):
		return errors.New("author differs")
	case b.Homepage != "" && doc.Homepage != b.Homepage:
		return errors.New("homepage differs")
	case b.Repository != "" && doc.Repository != b.Repository:
		return errors.New("repository differs")
	}
	return nil
}

func brandReadme(b *brand, base, t *target) string {
	shared := "the demarkus-plugin binary and the ~/.demarkus state"
	if base.Surface == "memory" {
		shared = fmt.Sprintf("the demarkus-plugin binary, the local memory (MCP server key %q), and the ~/.demarkus state", t.MCPServerKey)
	}
	return fmt.Sprintf(`# %s

%s

This plugin is generated from the %s plugin in the demarkus repository
(source: %s). Prompts, hooks, and scripts are identical apart from the plugin
name; it shares %s, so install it instead of, not alongside, %s.

Regenerate after editing the templates, the base plugin, or the brand entry,
from the tools/ directory of a demarkus checkout:

    go run ./plugin-prompts write [--brands <file>]
`, b.PluginName, b.Description, base.PluginName, base.Output, shared, base.PluginName)
}
