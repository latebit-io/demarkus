package gateway

import (
	"sync"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// Claims hang off a context that concurrent tool calls share, so the write
// gate must read them, never canonicalize in place.
func TestGateWriteDoesNotMutateClaims(t *testing.T) {
	cfg := &core.Config{Worlds: []core.WorldConfig{{
		Name:  "team-a",
		Allow: core.AllowConfig{Emails: []string{"alice@example.com"}},
	}}}
	g := newGatewayWithDispatcher(t, cfg, &fakeDispatcher{})
	raw := "  Alice@Example.com "
	claims := &core.Claims{Subject: "google|alice", Email: raw, EmailVerified: true}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			grant, err := g.writeGrantFor(claims, "team-a")
			if err != nil {
				t.Errorf("a canonically allowed writer was refused: %v", err)
			}
			if grant.Label != "alice@example.com" {
				t.Errorf("grant label = %q, want the canonical email", grant.Label)
			}
		})
	}
	wg.Wait()
	if claims.Email != raw {
		t.Errorf("claims.Email = %q, want it untouched", claims.Email)
	}
}
