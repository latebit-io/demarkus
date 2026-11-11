// Package depstest pins what a binary may link: its main package's dependency
// closure, checked in that module's own tests so the module's CI job runs it.
package depstest

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// cloud is what no binary on a user's machine links: cloud clients, gRPC,
// telemetry, OIDC, the model SDK and the Kubernetes API.
var cloud = []string{
	"cloud.google.com/", "google.golang.org/grpc", "go.opentelemetry.io/", "github.com/coreos/go-oidc",
	"github.com/anthropics/anthropic-sdk-go", "k8s.io/",
}

// Forbid fails t when the dependency closure of pattern, a go list pattern
// relative to the test's directory, holds a package under cloud or extra.
func Forbid(t *testing.T, pattern string, extra ...string) {
	t.Helper()
	linked, err := forbidden(pattern, extra)
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range linked {
		t.Errorf("%s links %s, which does not belong in this binary", pattern, link)
	}
}

// targets are the builds releases ship, cgo off as they are: a dependency
// only one of them links is still linked.
var targets = [][]string{
	{"GOOS=linux", "GOARCH=amd64"},
	{"GOOS=linux", "GOARCH=arm64"},
	{"GOOS=linux", "GOARCH=arm", "GOARM=7"},
	{"GOOS=darwin", "GOARCH=amd64"},
	{"GOOS=darwin", "GOARCH=arm64"},
}

// forbidden lists one linked package for each forbidden prefix the closure
// of pattern reaches on any release target.
func forbidden(pattern string, extra []string) ([]string, error) {
	var deps []string
	for _, target := range targets {
		cmd := exec.Command("go", "list", "-deps", pattern) //nolint:gosec // fixed binary; the pattern is the calling test's own
		cmd.Env = append(append(os.Environ(), "CGO_ENABLED=0"), target...)
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list -deps %s for %v: %w", pattern, target, err)
		}
		deps = append(deps, strings.Split(strings.TrimSpace(string(out)), "\n")...)
	}
	var linked []string
	for _, prefix := range slices.Concat(cloud, extra) {
		for _, dep := range deps {
			if strings.HasPrefix(dep, prefix) {
				linked = append(linked, dep)
				break
			}
		}
	}
	return linked, nil
}
