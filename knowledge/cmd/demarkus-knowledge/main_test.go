package main

import (
	"strings"
	"testing"
)

// The flags that refuse before anything opens: deprovisioning needs the
// broker's registry, serving needs the worlds; the broker itself is optional.
func TestRunRefusesIncompleteFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"deprovision without the broker", []string{"-deprovision-tenant", "acme"}, "-deprovision-tenant requires -broker-config"},
		{"delete-bucket without deprovision", []string{"-broker-config", "b.yaml", "-delete-bucket"}, "-delete-bucket requires -deprovision-tenant"},
		{"broker without worlds", []string{"-broker-config", "b.yaml"}, "-server-config is required"},
		{"nothing", nil, "-server-config is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(test.args)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("run(%v) = %v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

// Worlds alone: a server config and no broker config reaches the server's
// open, which is the first thing that reads the file.
func TestRunServesWorldsWithoutABroker(t *testing.T) {
	err := run([]string{"-server-config", "/nonexistent/server.yaml"})
	if err == nil || !strings.Contains(err.Error(), "server.yaml") {
		t.Fatalf("run with worlds alone = %v; want the server config read, not a broker refusal", err)
	}
}
