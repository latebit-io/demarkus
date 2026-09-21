package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// A manifest that could not be read is not a missing manifest: force
// overrides the second, never the first.
func TestCheckIndexManifestsTransportFailure(t *testing.T) {
	tests := []struct {
		name        string
		failWorld   string
		wantBlock   bool
		wantWarning string
	}{
		{name: "source unreachable warns with the cause", failWorld: "team-a", wantWarning: "could not check source"},
		{name: "target unreachable blocks despite force", failWorld: "hub", wantBlock: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDispatcher{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
				world := r.Host
				if world == tt.failWorld {
					return fetch.Result{}, errors.New("dial timeout")
				}
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
			}}
			g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
			warnings, block := g.checkIndexManifests(t.Context(), manifestCheck{sourceWorld: "team-a", targetWorld: "hub", force: true})
			if (block != nil) != tt.wantBlock {
				t.Fatalf("block = %v, want blocked=%v", block, tt.wantBlock)
			}
			if tt.wantBlock {
				if text := toolResultText(t, block); !strings.Contains(text, "dial timeout") {
					t.Errorf("block text = %q, want the cause", text)
				}
				return
			}
			if joined := strings.Join(warnings, "\n"); !strings.Contains(joined, tt.wantWarning) || !strings.Contains(joined, "dial timeout") {
				t.Errorf("warnings = %q, want %q with the cause", joined, tt.wantWarning)
			}
		})
	}
}

// force answers a missing target manifest only; any other status blocks.
func TestCheckIndexManifestsForceOnlyOverridesNotFound(t *testing.T) {
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
			d := &fakeDispatcher{FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
				world := r.Host
				if world == "hub" {
					return fetch.Result{Response: protocol.Response{Status: tt.status}}, nil
				}
				return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK}}, nil
			}}
			g := newGatewayWithDispatcher(t, mcpTestConfig(), d)
			_, block := g.checkIndexManifests(t.Context(), manifestCheck{sourceWorld: "team-a", targetWorld: "hub", force: true})
			if (block != nil) != tt.wantBlock {
				t.Fatalf("block = %v, want blocked=%v", block, tt.wantBlock)
			}
			if tt.wantBlock && !strings.Contains(toolResultText(t, block), tt.status) {
				t.Errorf("block text = %q, want the status", toolResultText(t, block))
			}
		})
	}
}
