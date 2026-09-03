package guidance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func ctx(t *testing.T, in Input) string {
	t.Helper()
	o, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return o.Context
}

func TestMemoryGuidanceHealthAndOffer(t *testing.T) {
	setupHome(t, map[string]string{"plugin-memory.conf": "SOUL_DIR=/no/such/dir\nPORT=6310\nMODE=default\n"})
	gfile := filepath.Join(t.TempDir(), "g.md")
	_ = os.WriteFile(gfile, []byte("# standing guidance"), 0o644)

	out := ctx(t, Input{Surface: "memory", GuidanceFile: gfile})
	if !strings.Contains(out, "server is not running") {
		t.Error("expected health warning (no .pid for the configured memory)")
	}
	if !strings.Contains(out, "One-time setup offer") {
		t.Error("expected the one-time memory offer on first call")
	}
	if !strings.Contains(out, "standing guidance") {
		t.Error("expected the static guidance body appended")
	}
	// second call: offer is now suppressed by the sentinel
	if strings.Contains(ctx(t, Input{Surface: "memory", GuidanceFile: gfile}), "One-time setup offer") {
		t.Error("offer should show only once")
	}
}

func TestKnowledgeGuidance(t *testing.T) {
	// no systems → one-time hint, then silent
	setupHome(t, nil)
	first := ctx(t, Input{Surface: "knowledge"})
	if !strings.Contains(first, "/knowledge-join") {
		t.Error("expected the one-time join hint when nothing is joined")
	}
	if ctx(t, Input{Surface: "knowledge"}) != "" {
		t.Error("hint should show only once")
	}

	// joined → lists the system
	setupHome(t, map[string]string{"knowledge-systems": "acme\nbeta\n"})
	gfile := filepath.Join(t.TempDir(), "kg.md")
	_ = os.WriteFile(gfile, []byte("# knowledge guidance"), 0o644)
	out := ctx(t, Input{Surface: "knowledge", GuidanceFile: gfile})
	if !strings.Contains(out, "**acme**") || !strings.Contains(out, "**beta**") {
		t.Errorf("expected joined systems listed, got: %s", out[:min(120, len(out))])
	}
	if !strings.Contains(out, "No local demarkus memory is configured") {
		t.Error("expected the memory↔system note when no memory is configured")
	}
}
