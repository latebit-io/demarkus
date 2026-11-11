package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestPluginCarriesNoCloud pins the plugin binary, which every session runs,
// to no cloud client, gRPC, telemetry or SDK, whatever tools/go.mod requires.
func TestPluginCarriesNoCloud(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, forbidden := range []string{
		"cloud.google.com/", "google.golang.org/grpc", "go.opentelemetry.io/", "github.com/coreos/go-oidc",
		"github.com/anthropics/anthropic-sdk-go", "k8s.io/",
	} {
		for _, dep := range deps {
			if strings.HasPrefix(dep, forbidden) {
				t.Errorf("demarkus-plugin links %s: nothing under %s belongs on a user's machine", dep, forbidden)
				break
			}
		}
	}
}
