package promote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/knowledge"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"
)

// detect-promote's output is parsed by three prompts; this pins its shape.
func TestDetectReportsEveryDestinationOrNone(t *testing.T) {
	home := registrytest.SetupHome(t)
	rows, err := Detect(true)
	if err != nil || Report(rows) != "NONE" {
		t.Fatalf("empty registry: %q, %v", Report(rows), err)
	}
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "knowledge-systems"), []byte("# joined\nteam\n\nlab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// lab joined before the url registry, so its row carries no host.
	if err := knowledge.Register("team", "https://Knowledge.Example.com/mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := AddTarget("acme", "/shared", "Acme shared"); err != nil {
		t.Fatal(err)
	}
	rows, err = Detect(true)
	if err != nil || Report(rows) != "knowledge team knowledge.example.com\nknowledge lab\ntarget acme /shared Acme shared" {
		t.Fatalf("Detect(hosts) = %q, %v", Report(rows), err)
	}
	// Older plugins' prompts reject a third field on a knowledge row.
	rows, err = Detect(false)
	if err != nil || Report(rows) != "knowledge team\nknowledge lab\ntarget acme /shared Acme shared" {
		t.Fatalf("Detect = %q, %v", Report(rows), err)
	}
	// Register refuses a hostless URL; a hand-edited registry can hold one.
	// It fails only the hosts form.
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "knowledge-urls"), []byte("lab\thttps:///mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(false); err != nil {
		t.Fatalf("Detect with a hostless URL: %v", err)
	}
	if _, err := Detect(true); err == nil {
		t.Fatal("Detect(hosts) with a hostless URL: want error")
	}
}

func TestPromoteTargetAdd(t *testing.T) {
	home := registrytest.SetupHome(t)
	if _, err := AddTarget("acme", "/shared", "Acme shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := AddTarget("acme", "bad", ""); err == nil {
		t.Error("path not starting with / should error")
	}
	for _, unsafe := range []string{"/docs/../secret", "/docs/./internal", `/docs\secret`, "/safe\nother", "/safe\rother"} {
		if _, err := AddTarget("acme", unsafe, ""); err == nil {
			t.Errorf("unsafe path %q should error", unsafe)
		}
	}
	for _, label := range []string{"bad\tlabel", "bad\rlabel", "bad\nlabel", "bad\x00label", " leading", "trailing "} {
		if _, err := AddTarget("acme", "/labeled", label); err == nil {
			t.Errorf("unsafe label %q should error", label)
		}
	}
	canonical, err := AddTarget("acme", "/shared//nested/", "Nested")
	if err != nil {
		t.Fatal(err)
	}
	if canonical != "/shared/nested" {
		t.Fatalf("canonical path = %q", canonical)
	}
	rows, err := config.PromoteTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0] != "acme /shared Acme shared" || rows[1] != "acme /shared/nested Nested" {
		t.Fatalf("promote target rows = %v", rows)
	}
	registryPath := filepath.Join(home, ".demarkus", "promote-targets")
	legacyRows := strings.Join(append(rows, "legacy /legacy//nested/ Legacy label"), "\n") + "\n"
	if err := os.WriteFile(registryPath, []byte(legacyRows), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AddTarget("legacy", "/legacy/nested", "Ignored replacement"); err != nil {
		t.Fatal(err)
	}
	rows, err = config.PromoteTargets()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2] != "legacy /legacy/nested Legacy label" {
		t.Fatalf("promote target row wrong: %v", rows)
	}
}
