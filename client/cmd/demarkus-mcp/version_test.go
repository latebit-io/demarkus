package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildAndVersion compiles this package into a temp binary (optionally injecting
// main.version via -ldflags, exactly as the release build does) and runs it with
// the given version arg, returning trimmed stdout. It proves both the flag wiring
// and that the -X main.version target is correct — a wrong package path would
// silently leave the binary reporting "dev".
func buildAndVersion(t *testing.T, ldVersion string, args ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "demarkus-mcp")
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

func TestVersionFlag(t *testing.T) {
	tests := []struct {
		name      string
		ldVersion string
		want      string
	}{
		{"injected release version", "9.9.9-test", "9.9.9-test"},
		{"default when not injected", "", "dev"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildAndVersion(t, tt.ldVersion, "--version"); got != tt.want {
				t.Errorf("--version = %q, want %q", got, tt.want)
			}
		})
	}
}
