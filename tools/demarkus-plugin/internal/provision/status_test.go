package provision

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/procscan"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/release"
)

// Without a config, status still reports the pins and every binary, so the
// status command needs no shell loop of its own.
func TestStatusReportsBinariesWithoutAConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := Status()
	if err != nil {
		t.Fatal(err)
	}
	want := "no local memory configured (run /soul-init)\ndesired=" + release.DesiredVersions() +
		"\nbinary demarkus-server missing\nbinary demarkus-mcp missing\nbinary demarkus-token missing"
	if got != want {
		t.Fatalf("Status =\n%s\nwant\n%s", got, want)
	}
}

// A detected server is reusable only with a root that still exists; the rest
// is listed, with its reason, never silently dropped.
func TestDetectReportSkipsServersNoPromptMayReuse(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "gone")
	servers := []procscan.Server{
		{PID: 11, Port: "6309", Root: root},
		{PID: 12, Port: "6310"},
		{PID: 13, Port: "6311", Root: gone},
		{PID: 14, Port: "6312", Root: file},
	}
	want := "SERVERS\n11 6309 " + root + "\nSKIPPED\n" +
		"12 6310 (unknown) (root unknown)\n" +
		"13 6311 " + gone + " (root is gone)\n" +
		"14 6312 " + file + " (root is not a directory)"
	if got := detectReport(servers); got != want {
		t.Fatalf("detectReport =\n%s\nwant\n%s", got, want)
	}
	if got := detectReport(nil); got != "NO_SERVER" {
		t.Errorf("no servers = %q", got)
	}
	if got := detectReport(servers[1:2]); got != "NO_SERVER\nSKIPPED\n12 6310 (unknown) (root unknown)" {
		t.Errorf("only skipped servers = %q", got)
	}
}

// A hook's provision never runs the default setup: without a config there is
// nothing to do, and no file appears.
func TestIfConfiguredDoesNothingWithoutAConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := IfConfigured(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".demarkus", "plugin-memory.conf")); err == nil {
		t.Fatal("a hook's provision wrote a config")
	}
}
