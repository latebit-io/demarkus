package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildAndVersion compiles this package into a temp binary (optionally injecting
// main.version via -ldflags, exactly as the release build does) and runs the
// given version subcommand, returning trimmed stdout. It proves both the
// subcommand wiring and that the -X main.version target is correct — a wrong
// package path would silently leave the binary reporting "dev".
func buildAndVersion(t *testing.T, ldVersion string, args ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "demarkus-token")
	buildArgs := []string{"build", "-o", bin}
	if ldVersion != "" {
		buildArgs = append(buildArgs, "-ldflags", "-X main.version="+ldVersion)
	}
	buildArgs = append(buildArgs, ".")
	if out, err := exec.Command("go", buildArgs...).CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		t.Fatalf("run %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func TestVersionSubcommand(t *testing.T) {
	tests := []struct {
		name      string
		ldVersion string
		arg       string
		want      string
	}{
		{"version subcommand, injected", "9.9.9-test", "version", "9.9.9-test"},
		{"--version flag form", "9.9.9-test", "--version", "9.9.9-test"},
		{"default when not injected", "", "version", "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildAndVersion(t, tt.ldVersion, tt.arg); got != tt.want {
				t.Errorf("%s = %q, want %q", tt.arg, got, tt.want)
			}
		})
	}
}
