package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestClientBinariesCarryNoServer pins the client binaries (CLI, MCP, TUI) to
// the store client: no server module, cloud client, gRPC, telemetry or SDK.
func TestClientBinariesCarryNoServer(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../...").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, forbidden := range []string{
		"cloud.google.com/", "google.golang.org/grpc", "go.opentelemetry.io/", "github.com/coreos/go-oidc",
		"github.com/anthropics/anthropic-sdk-go", "k8s.io/", "github.com/latebit-io/demarkus/server",
	} {
		for _, dep := range deps {
			if strings.HasPrefix(dep, forbidden) {
				t.Errorf("a client binary links %s: nothing under %s belongs on a user's machine", dep, forbidden)
				break
			}
		}
	}
}
