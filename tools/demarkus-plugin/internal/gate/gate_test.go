package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// setupHome points ~/.demarkus at a temp dir and writes the given fixture files
// (relative to ~/.demarkus). Returns the home dir.
func setupHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dm := filepath.Join(home, ".demarkus")
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dm, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func mustEval(t *testing.T, in Input) Decision {
	t.Helper()
	d, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate error: %v", err)
	}
	return d
}

func TestParseTool(t *testing.T) {
	cases := []struct {
		tool       string
		wantServer string
		wantVerb   string
		wantParsed bool
	}{
		{"demarkus_memory_mark_publish", "demarkus_memory", "publish", true},
		{"mcp__demarkus-memory__mark_append", "demarkus-memory", "append", true},
		{"mark_fetch", "", "fetch", true},
		{"knowledge_mark_publish", "knowledge", "publish", true},
		{"read_file", "", "", false},
	}
	for _, c := range cases {
		pt, ok := config.ParseTool(c.tool)
		if ok != c.wantParsed {
			t.Errorf("%s: parsed=%v, want %v", c.tool, ok, c.wantParsed)
			continue
		}
		if ok && (pt.Server != c.wantServer || pt.Verb != c.wantVerb) {
			t.Errorf("%s: got {%q,%q}, want {%q,%q}", c.tool, pt.Server, pt.Verb, c.wantServer, c.wantVerb)
		}
	}
}

func TestPublishTagGate(t *testing.T) {
	t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})

	tagless := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{"url": "/x.md"}})
	if tagless.Decision != "block" {
		t.Fatalf("tagless: want block, got %q (%s)", tagless.Decision, tagless.Reason)
	}

	tagged := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/x.md", "metadata": map[string]any{"tags": "a,b"},
	}})
	if tagged.Decision != "allow" {
		t.Fatalf("tagged: want allow, got %q", tagged.Decision)
	}
}

func TestImportanceValidation(t *testing.T) {
	t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
	setupHome(t, nil)
	base := func(imp any) Input {
		return Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/x.md", "metadata": map[string]any{"tags": "a", "importance": imp},
		}}
	}
	cases := []struct {
		name string
		imp  any
		want string
	}{
		{"good float", 0.5, "allow"},
		{"numeric string", "0.8", "allow"},
		{"out of range", 5.0, "block"},
		{"blank string", "   ", "block"},
		{"array", []any{}, "block"},
		{"bool", true, "block"},
	}
	for _, c := range cases {
		if got := mustEval(t, base(c.imp)); got.Decision != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got.Decision)
		}
	}
	// absent importance is fine
	if got := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
		"url": "/x.md", "metadata": map[string]any{"tags": "a"},
	}}); got.Decision != "allow" {
		t.Errorf("absent importance: want allow, got %s", got.Decision)
	}
}

func TestDestinationGate(t *testing.T) {
	home := setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	repo := filepath.Join(home, "repo")
	sub := filepath.Join(repo, "subdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// bind repo → a different soul; a write to the local soul from repo/subdir misroutes.
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "project-souls"), []byte(repo+"\tother\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Cwd: sub, Input: map[string]any{
		"url": "/x.md", "metadata": map[string]any{"tags": "a"},
	}})
	if d.Decision != "block" { // dest strictness defaults to block
		t.Fatalf("misroute from subdir: want block, got %q (%s)", d.Decision, d.Reason)
	}
	// a write to the bound soul is fine — but "other" isn't a registered remote
	// soul here, so it classifies as unrelated → allow. Use the bound case where
	// target == bound by binding to the local soul instead.
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "project-souls"), []byte(repo+"\tdemarkus-memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2 := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Cwd: sub, Input: map[string]any{
		"url": "/x.md", "metadata": map[string]any{"tags": "a"},
	}})
	if d2.Decision != "allow" {
		t.Fatalf("bound-soul write: want allow, got %q", d2.Decision)
	}
}

// A warn-level destination misroute must NOT suppress a block-level tag gate:
// the two gates are independent and the most severe outcome wins.
func TestMostSevereWins(t *testing.T) {
	t.Setenv("DEMARKUS_MEMORY_DEST_STRICTNESS", "warn")
	t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
	home := setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "project-souls"), []byte(repo+"\tother\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// misroute (dest=warn) + tagless (tag=block) → block, tag reason present
	d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Cwd: repo, Input: map[string]any{"url": "/x.md"}})
	if d.Decision != "block" {
		t.Fatalf("want block (tag gate wins over warn dest), got %q", d.Decision)
	}
	if !strings.Contains(d.Reason, "metadata.tags") {
		t.Fatalf("expected tag-gate reason in combined output, got: %s", d.Reason)
	}
}

func TestKnowledgeGate(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "block")
	setupHome(t, map[string]string{
		"knowledge-systems":                         "knowledge\n",
		"plugin-knowledge.require-fields.knowledge": "type\n",
		"plugin-knowledge.require-tags.knowledge":   "category\n",
	})
	// tagless → block
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{"url": "/k.md"}}); d.Decision != "block" {
		t.Fatalf("ks tagless: want block, got %q", d.Decision)
	}
	// tags present but missing required axis (category:) and required field type → block
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
		"url": "/k.md", "metadata": map[string]any{"tags": "topic"},
	}}); d.Decision != "block" {
		t.Fatalf("ks missing axis/type: want block, got %q", d.Decision)
	}
	// satisfies axis + type → allow
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
		"url": "/k.md", "metadata": map[string]any{"tags": "category:project,topic", "type": "Reference"},
	}}); d.Decision != "allow" {
		t.Fatalf("ks compliant: want allow, got %q (%s)", d.Decision, d.Reason)
	}
	// index.md is exempt from required type
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
		"url": "/world/index.md", "metadata": map[string]any{"tags": "category:project"},
	}}); d.Decision != "allow" {
		t.Fatalf("ks index.md exempt: want allow, got %q (%s)", d.Decision, d.Reason)
	}
}

// A policy-declared required field other than `type` must be enforced too — the
// binary has the full metadata, so it checks any field generically.
func TestKnowledgeRequiresArbitraryField(t *testing.T) {
	t.Setenv("DEMARKUS_KNOWLEDGE_STRICTNESS", "block")
	setupHome(t, map[string]string{
		"knowledge-systems":                         "knowledge\n",
		"plugin-knowledge.require-fields.knowledge": "authors\n",
	})
	// missing authors → block
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
		"url": "/k.md", "metadata": map[string]any{"tags": "topic"},
	}}); d.Decision != "block" {
		t.Fatalf("missing required 'authors': want block, got %q", d.Decision)
	}
	// authors present → allow
	if d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
		"url": "/k.md", "metadata": map[string]any{"tags": "topic", "authors": "ada"},
	}}); d.Decision != "allow" {
		t.Fatalf("authors present: want allow, got %q (%s)", d.Decision, d.Reason)
	}
}

func TestMcpProxyUnwrap(t *testing.T) {
	t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
	setupHome(t, nil)
	// pi-mcp-adapter proxy: tool "mcp", real call in input.tool/input.args (JSON string)
	args, _ := json.Marshal(map[string]any{"url": "/x.md"})
	d := mustEval(t, Input{Tool: "mcp", Input: map[string]any{
		"tool": "demarkus_memory_mark_publish",
		"args": string(args),
	}})
	if d.Decision != "block" {
		t.Fatalf("proxy tagless: want block, got %q (%s)", d.Decision, d.Reason)
	}
}

func TestUnrelatedServerAllowed(t *testing.T) {
	t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
	setupHome(t, nil)
	// not the local soul, not a registered remote soul or knowledge system → allow
	if d := mustEval(t, Input{Tool: "someother_mark_publish", Input: map[string]any{"url": "/x.md"}}); d.Decision != "allow" {
		t.Fatalf("unrelated server: want allow, got %q", d.Decision)
	}
}

func TestRetentionGate(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/x\nPORT=6310\nMODE=default\n"})

	withRetention := func(tool string, retention any) Input {
		return Input{Tool: tool, Input: map[string]any{
			"url": "/x.md", "metadata": map[string]any{"tags": "a,b", "retention": retention},
		}}
	}

	t.Run("memory publish asks by default", func(t *testing.T) {
		d := mustEval(t, withRetention("demarkus_memory_mark_publish", "20"))
		if d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
		if !strings.Contains(d.Reason, "retention=20") || !strings.Contains(d.Reason, "permanently delete") {
			t.Fatalf("reason should explain the deletion: %s", d.Reason)
		}
	})

	t.Run("memory append asks too", func(t *testing.T) {
		if d := mustEval(t, withRetention("demarkus_memory_mark_append", "5")); d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("float64 retention asks", func(t *testing.T) {
		// The default JSON decode yields float64 for numbers.
		if d := mustEval(t, withRetention("demarkus_memory_mark_publish", 20.0)); d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("json.Number retention asks", func(t *testing.T) {
		// Adapters decoding with UseNumber hand the gate a json.Number.
		if d := mustEval(t, withRetention("demarkus_memory_mark_publish", json.Number("20"))); d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("native int retention asks", func(t *testing.T) {
		// Programmatic callers can build the metadata map with a Go int; it
		// reaches the server as "20" and prunes, so the gate must catch it.
		if d := mustEval(t, withRetention("demarkus_memory_mark_publish", 20)); d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
		if d := mustEval(t, withRetention("demarkus_memory_mark_publish", int64(20))); d.Decision != "ask" {
			t.Fatalf("int64: want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("server-rejectable values pass through", func(t *testing.T) {
		// None of these can prune — the server rejects them with bad-request,
		// so a destructive prompt would be misleading.
		for _, v := range []any{"0", "-1", "abc", "", 0.0, -3.0, 20.5, true, nil, 0, -2, int64(0)} {
			if d := mustEval(t, withRetention("demarkus_memory_mark_publish", v)); d.Decision != "allow" {
				t.Errorf("retention=%v: want allow, got %q (%s)", v, d.Decision, d.Reason)
			}
		}
	})

	t.Run("absent retention allows", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/x.md", "metadata": map[string]any{"tags": "a,b"},
		}})
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("graph publish is exempt", func(t *testing.T) {
		// mark_graph_publish sets retention by design on a generated document;
		// its verb parses as "graph_publish" so neither write gate fires.
		d := mustEval(t, withRetention("demarkus_memory_mark_graph_publish", "20"))
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("strictness env override", func(t *testing.T) {
		t.Setenv("DEMARKUS_RETENTION_STRICTNESS", "warn")
		if d := mustEval(t, withRetention("demarkus_memory_mark_publish", "20")); d.Decision != "warn" {
			t.Fatalf("want warn, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("block-level tag gate outranks retention ask", func(t *testing.T) {
		t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "block")
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/x.md", "metadata": map[string]any{"retention": "20"},
		}})
		if d.Decision != "block" {
			t.Fatalf("want block, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("retention ask outranks warn-level tag gate", func(t *testing.T) {
		t.Setenv("DEMARKUS_MEMORY_STRICTNESS", "warn")
		d := mustEval(t, Input{Tool: "demarkus_memory_mark_publish", Input: map[string]any{
			"url": "/x.md", "metadata": map[string]any{"retention": "20"},
		}})
		if d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
		if !strings.Contains(d.Reason, "retention") {
			t.Fatalf("expected retention reason at winning severity, got: %s", d.Reason)
		}
	})
}

func TestRetentionGateKnowledge(t *testing.T) {
	setupHome(t, map[string]string{"knowledge-systems": "knowledge\n"})

	t.Run("compliant publish with retention asks", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "knowledge_mark_publish", Input: map[string]any{
			"url": "/k.md", "metadata": map[string]any{"tags": "a,b", "retention": "10"},
		}})
		if d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("append with retention asks", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "knowledge_mark_append", Input: map[string]any{
			"url": "/k.md", "metadata": map[string]any{"retention": "10"},
		}})
		if d.Decision != "ask" {
			t.Fatalf("want ask, got %q (%s)", d.Decision, d.Reason)
		}
	})

	t.Run("append without retention allows", func(t *testing.T) {
		d := mustEval(t, Input{Tool: "knowledge_mark_append", Input: map[string]any{
			"url": "/k.md", "body": "more",
		}})
		if d.Decision != "allow" {
			t.Fatalf("want allow, got %q (%s)", d.Decision, d.Reason)
		}
	})
}

func TestPrunableRetentionJSONNumber(t *testing.T) {
	// Adapters that decode with json.Number must be recognized too.
	if v, ok := prunableRetention(map[string]any{"retention": json.Number("15")}); !ok || v != "15" {
		t.Errorf("json.Number 15: got (%q, %v), want (15, true)", v, ok)
	}
	if _, ok := prunableRetention(map[string]any{"retention": json.Number("0")}); ok {
		t.Error("json.Number 0 should not be prunable")
	}
}
