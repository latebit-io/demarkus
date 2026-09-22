package promote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"
)

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
