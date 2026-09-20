package broker

import (
	"sync"
	"testing"
)

// Claims hang off a context that concurrent tool calls share, so the write
// gate must read them, never canonicalize in place.
func TestGateWriteDoesNotMutateClaims(t *testing.T) {
	cfg := &Config{Worlds: []WorldConfig{{
		Name:  "team-a",
		Allow: AllowConfig{Emails: []string{"alice@example.com"}},
	}}}
	g := newGatewayWithDispatcher(t, cfg, nil)
	raw := "  Alice@Example.com "
	claims := &Claims{Subject: "google|alice", Email: raw, EmailVerified: true}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, denied := g.gateWrite(claims, "team-a"); denied != nil {
				t.Errorf("gateWrite denied a canonically allowed writer")
			}
		})
	}
	wg.Wait()
	if claims.Email != raw {
		t.Errorf("claims.Email = %q, want it untouched", claims.Email)
	}
}
