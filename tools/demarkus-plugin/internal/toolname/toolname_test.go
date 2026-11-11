package toolname

import (
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"
)

func TestParse(t *testing.T) {
	cases := []struct {
		tool, server, verb string
		parsed             bool
	}{
		{"demarkus_memory_mark_publish", "demarkus_memory", "publish", true},
		{"mcp__demarkus-memory__mark_append", "demarkus-memory", "append", true},
		{"mark_fetch", "", "fetch", true},
		{"knowledge_mark_publish", "knowledge", "publish", true},
		{"mcp__corp__mark_graph_publish", "corp", "graph_publish", true},
		{"read_file", "", "", false},
		// A server name holding "mark_" once shifted the verb and slipped every gate.
		{"mcp__bookmark__mark_publish", "bookmark", "publish", true},
		{"mcp__mark_soul__mark_publish", "mark_soul", "publish", true},
		{"mark_soul_mark_append", "mark_soul", "append", true},
		{"benchmark_notes_mark_publish", "benchmark_notes", "publish", true},
		{"mcp__benchmark-notes__mark_publish", "benchmark-notes", "publish", true},
		{"bookmark_publish", "", "", false},
	}
	for _, c := range cases {
		pt, ok := Parse(c.tool)
		if ok != c.parsed || pt.Server != c.server || pt.Verb != c.verb {
			t.Errorf("Parse(%q) = {%q,%q} %v, want {%q,%q} %v", c.tool, pt.Server, pt.Verb, ok, c.server, c.verb, c.parsed)
		}
	}
}

func TestQualify(t *testing.T) {
	for _, c := range []struct{ tool, server, want string }{
		{"mark_publish", "soul", "mcp__soul__mark_publish"},
		{"mcp__soul__mark_publish", "other", "mcp__soul__mark_publish"},
		{"mark_publish", "", "mark_publish"},
		{"", "soul", ""},
	} {
		if got := Qualify(c.tool, c.server); got != c.want {
			t.Errorf("Qualify(%q, %q) = %q, want %q", c.tool, c.server, got, c.want)
		}
	}
}

// A joined store whose slug holds "mark" routes like any other.
func TestRoutingSurvivesMarkInTheServerName(t *testing.T) {
	registrytest.SetupHomeWith(t, map[string]string{"souls": "bookmark\tmark://b.example\t0\t-\n", "knowledge-systems": "benchmark-notes\n"})
	if id, err := MemoryTargetID("mcp__bookmark__mark_publish"); err != nil || id != "bookmark" {
		t.Errorf("memory target = %q, %v, want bookmark", id, err)
	}
	if slug, err := KnowledgeScope("benchmark_notes_mark_publish"); err != nil || slug != "benchmark-notes" {
		t.Errorf("knowledge scope = %q, %v, want benchmark-notes", slug, err)
	}
}

func TestLocalMemoryAliasResolvesBrandedServer(t *testing.T) {
	branded := "mcp__plugin_acme-brain_memory__mark_publish"
	registrytest.SetupHomeWith(t, map[string]string{"knowledge-systems": "corp\n"})
	if id, err := MemoryTargetID(branded); err != nil || id != "" {
		t.Fatalf("no alias: id=%q err=%v", id, err)
	}
	registrytest.SetupHomeWith(t, map[string]string{"knowledge-systems": "corp\n", config.LocalMemoryAliasFile: "0123abcd memory\n"})
	for _, tool := range []string{branded, "mcp__memory__mark_append", "memory_mark_publish", "mcp__plugin_demarkus-memory_demarkus-memory__mark_publish"} {
		id, err := MemoryTargetID(tool)
		if err != nil || id != config.LocalMemoryID {
			t.Fatalf("%s: id=%q err=%v", tool, id, err)
		}
		scope, err := KnowledgeScope(tool)
		if err != nil || scope != "" {
			t.Fatalf("%s: knowledge scope=%q err=%v", tool, scope, err)
		}
		if local, err := TargetsMemory(tool); err != nil || !local {
			t.Fatalf("%s: TargetsMemory = %v, %v", tool, local, err)
		}
	}
	if scope, err := KnowledgeScope("mcp__corp__mark_publish"); err != nil || scope != "corp" {
		t.Fatalf("knowledge tool: scope=%q err=%v", scope, err)
	}
}

func TestIsJournalWrite(t *testing.T) {
	for _, c := range []struct {
		tool, url, route string
		want             bool
	}{
		{"mcp__soul__mark_append", "/p/", "today", true},
		{"mark_publish", "/p/journal/2026-01-01.md", "", true},
		{"mark_publish", "/p/roadmap.md", "", false},
		{"mark_fetch", "/p/journal/2026-01-01.md", "", false},
	} {
		if got := IsJournalWrite(c.tool, c.url, c.route); got != c.want {
			t.Errorf("IsJournalWrite(%q, %q, %q) = %v", c.tool, c.url, c.route, got)
		}
	}
}
