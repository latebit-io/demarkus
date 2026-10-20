package hosttools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/client/mcpfmt/mcpfmttest"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"
)

// Wire bytes of the two provided schemas, paid on every plugin session on
// top of the lean profile (1,549 on 2026-10-20). Raise only with a measured offset.
const schemaBudgetBytes = 1600

func TestHostToolsGolden(t *testing.T) {
	mcpfmttest.ToolsGolden(t, "testdata/tools.json", Tools())
}

func TestHostToolsSchemaBudget(t *testing.T) {
	if err := mcpfmt.CheckSchemaBudget(Tools(), schemaBudgetBytes); err != nil {
		t.Fatal(err)
	}
}

// The schema's tools are the dispatch table's, and none is in the lean
// profile's exclusions, so a prompt may name them.
func TestToolsMatchTheTable(t *testing.T) {
	for _, tool := range Tools() {
		if _, ok := tools[tool.Name]; !ok {
			t.Errorf("%s has no dispatch", tool.Name)
		}
		if mcpfmt.AdvancedTools[tool.Name] {
			t.Errorf("%s is advanced; plugin prompts cannot name it", tool.Name)
		}
	}
	if len(Tools()) != len(tools) {
		t.Errorf("%d schemas for %d tables", len(Tools()), len(tools))
	}
}

func TestCallRefusesWhatTheSchemaDoesNotOffer(t *testing.T) {
	registrytest.SetupHome(t)
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown tool", "mark_other", map[string]any{"action": "status"}, "unknown host tool"},
		{"no action", ReadTool, map[string]any{}, `required argument "action" not found`},
		{"unknown action", SetupTool, map[string]any{"action": "nope"}, `unknown action "nope"`},
		{"project without dir", ReadTool, map[string]any{"action": "project"}, `project: required argument "dir" not found`},
		{"dir not a string", ReadTool, map[string]any{"action": "catalog", "dir": 3}, `catalog: argument "dir" is not a string`},
		{"init without mode", SetupTool, map[string]any{"action": "init"}, `init: required argument "mode" not found`},
		{"default without slug", SetupTool, map[string]any{"action": "default", "dir": "/p"}, `default: required argument "slug" not found`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Call(tt.tool, tt.args)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// The read actions answer with the CLI's text over an empty home: the same
// lines the prompts already parse.
func TestReadActionsAnswerLikeTheCLI(t *testing.T) {
	registrytest.SetupHome(t)
	got, err := Call(ReadTool, map[string]any{"action": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "no local memory configured (run /soul-init)\ndesired="+release.DesiredVersions()) {
		t.Errorf("status = %q", got)
	}
	got, err = Call(ReadTool, map[string]any{"action": "promote"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "NONE" {
		t.Errorf("promote = %q", got)
	}
}

// default binds through the same catalog write the CLI uses; the binding
// then stars the row.
func TestDefaultBindsTheProject(t *testing.T) {
	registrytest.SetupHome(t)
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "soul", Host: "mark://soul.example"})
	dir := filepath.Join(t.TempDir(), "proj")
	if _, err := Call(SetupTool, map[string]any{"action": "default", "dir": dir, "slug": "nope"}); err == nil || !strings.Contains(err.Error(), "not a joined memory") {
		t.Fatalf("binding an unjoined memory: %v", err)
	}
	got, err := Call(SetupTool, map[string]any{"action": "default", "dir": dir, "slug": "soul"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "OK soul" {
		t.Errorf("default = %q", got)
	}
	got, err = Call(ReadTool, map[string]any{"action": "catalog", "dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != "CATALOG\nsoul\tremote\tmark://soul.example\t0\t*" {
		t.Errorf("catalog after bind = %q", got)
	}
}

func TestDefaultListingFlagsStaleBinding(t *testing.T) {
	cat := []string{"demarkus-memory\tlocal\t-\t-", "soul\tremote\tmark://s\t0"}
	got := strings.Join(DefaultListing(cat, "soul"), "\n")
	if !strings.Contains(got, "soul\tremote\tmark://s\t0\t*") || strings.Contains(got, "STALE") {
		t.Fatalf("bound row should be starred, no STALE: %q", got)
	}
	got = strings.Join(DefaultListing(cat, "gone"), "\n")
	if !strings.HasSuffix(got, "\nSTALE gone") || strings.Contains(got, "\t*") {
		t.Fatalf("missing binding should end with STALE and star nothing: %q", got)
	}
	if got := strings.Join(DefaultListing(nil, "gone"), "\n"); got != "EMPTY\nSTALE gone" {
		t.Fatalf("empty catalog with a binding: %q", got)
	}
	if got := strings.Join(DefaultListing(nil, ""), "\n"); got != "EMPTY" {
		t.Fatalf("unbound empty: %q", got)
	}
}
