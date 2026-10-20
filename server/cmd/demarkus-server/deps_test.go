package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestLocalMainCarriesNoCloud pins ADR 0037 decision 6: the soul's binary is
// the local profile of the shared runtime and never links a cloud client,
// gRPC, telemetry or OAuth, whatever the knowledge server grows.
func TestLocalMainCarriesNoCloud(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, forbidden := range []string{"cloud.google.com/", "google.golang.org/grpc", "go.opentelemetry.io/", "github.com/coreos/go-oidc"} {
		for _, dep := range deps {
			if strings.HasPrefix(dep, forbidden) {
				t.Errorf("demarkus-server links %s: the local profile must carry nothing under %s", dep, forbidden)
				break
			}
		}
	}
}
