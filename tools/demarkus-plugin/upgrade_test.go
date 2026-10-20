package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"
)

// TestReadsUpgradeCapture reads the ~/.demarkus a released plugin wrote
// (testdata/upgrade, scripts/capture-upgrade-fixture.sh) with the current
// plugin: config, catalog, bindings, knowledge registry and promote targets.
func TestReadsUpgradeCapture(t *testing.T) {
	home := installCaptureHome(t, filepath.Join("..", "..", "testdata", "upgrade"))

	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		t.Fatalf("LoadConfig = %+v, %v", cfg, err)
	}
	if cfg.Mode != "default" || cfg.MemoryDir != filepath.Join(home, ".demarkus", "soul") {
		t.Errorf("config = %+v", cfg)
	}

	local, err := catalog.Resolve(config.LocalMemoryID)
	if err != nil || local.Host != "mark://localhost:"+cfg.Port || local.Token == "" {
		t.Errorf("local endpoint = %+v, %v", local, err)
	}
	team, err := catalog.Resolve("team")
	if err != nil || team.Host != "mark://team.example.com:6309" || team.Token != "fixture-remote-token" {
		t.Errorf("joined endpoint = %+v, %v", team, err)
	}

	for dir, store := range map[string]string{"project": "team", "other-project": "lab"} {
		r, err := project.Resolve(filepath.Join(home, dir))
		if err != nil || r.Store != store || r.State != project.StateBound {
			t.Errorf("%s resolves to %+v, %v; want bound to %s", dir, r, err, store)
		}
	}

	systems, err := config.ListKnowledgeSystems()
	if err != nil || !slices.Equal(systems, []string{"acme"}) {
		t.Errorf("knowledge systems = %v, %v", systems, err)
	}
	targets, err := config.PromoteTargets()
	if err != nil || len(targets) != 1 || !strings.HasPrefix(targets[0], "archive ") {
		t.Errorf("promote targets = %v, %v", targets, err)
	}
}

// installCaptureHome copies a capture's home into a temp HOME, rebasing the
// absolute paths the plugin recorded under the home named in CAPTURE.
func installCaptureHome(t *testing.T, capture string) string {
	t.Helper()
	recorded := captureField(t, capture, "home")
	home := registrytest.SetupHome(t)
	src := filepath.Join(capture, "home")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(home, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, []byte(strings.ReplaceAll(string(raw), recorded, home)), 0o600)
	})
	if err != nil {
		t.Fatalf("install capture: %v", err)
	}
	for _, dir := range []string{"project", "other-project"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// captureField reads one "key: value" line of the capture's CAPTURE file.
func captureField(t *testing.T, capture, key string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(capture, "CAPTURE"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(raw)) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+": "); ok {
			return value
		}
	}
	t.Fatalf("%s/CAPTURE has no %s", capture, key)
	return ""
}
