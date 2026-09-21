package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// A manifest that could not be read is not a missing manifest: force
// overrides the second, never the first.
func TestCheckManifestsTransportFailure(t *testing.T) {
	tests := []struct {
		name        string
		failHost    string
		wantBlock   bool
		wantWarning string
	}{
		{name: "source unreachable warns with the cause", failHost: "source.com:6309", wantWarning: "could not check source"},
		{name: "target unreachable blocks despite force", failHost: "hub.com:6309", wantBlock: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &stubClient{FetchFn: func(host, _, _ string) (fetch.Result, error) {
				if host == tt.failHost {
					return fetch.Result{}, errors.New("dial timeout")
				}
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
			}}
			h := &handler{client: sc}
			warnings, block := h.checkManifests("source.com:6309", "hub.com:6309", false, true)
			if (block != nil) != tt.wantBlock {
				t.Fatalf("block = %v, want blocked=%v", block, tt.wantBlock)
			}
			if tt.wantBlock {
				assertIsToolError(t, block, "dial timeout")
				return
			}
			if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, tt.wantWarning) || !strings.Contains(joined, "dial timeout") {
				t.Errorf("warnings = %q, want %q with the cause", joined, tt.wantWarning)
			}
		})
	}
}

// force answers a missing target manifest only; any other status blocks.
func TestCheckManifestsForceOnlyOverridesNotFound(t *testing.T) {
	tests := []struct {
		status    string
		wantBlock bool
	}{
		{protocol.StatusNotFound, false},
		{protocol.StatusUnauthorized, true},
		{protocol.StatusNotPermitted, true},
		{protocol.StatusServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			sc := &stubClient{FetchFn: func(host, _, _ string) (fetch.Result, error) {
				if host == "hub.com:6309" {
					return fetch.Result{Response: protocol.Response{Status: tt.status}}, nil
				}
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
			}}
			_, block := (&handler{client: sc}).checkManifests("source.com:6309", "hub.com:6309", false, true)
			if (block != nil) != tt.wantBlock {
				t.Fatalf("block = %v, want blocked=%v", block, tt.wantBlock)
			}
			if tt.wantBlock {
				assertIsToolError(t, block, tt.status)
			}
		})
	}
}
