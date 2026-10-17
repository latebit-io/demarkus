package graphstore

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestSeedGateBoundsAndBacksOff(t *testing.T) {
	var gate SeedGate
	runs := 0
	pass := func(ctx context.Context) bool {
		runs++
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("pass must run under SeedTimeout")
		}
		return false
	}
	gate.Run(t.Context(), "host", pass)
	gate.Run(t.Context(), "host", pass)
	if runs != 1 {
		t.Fatalf("runs = %d, want 1: a failed pass backs off", runs)
	}
	if _, checked := gate.LastCheck("host"); !checked {
		t.Fatal("failed pass must record a check")
	}
	gate.Expire("host")
	gate.Run(t.Context(), "host", pass)
	if runs != 2 {
		t.Fatalf("runs = %d, want 2 after Expire", runs)
	}
}

func TestSeedGateCallerCancelDoesNotCountAsCheck(t *testing.T) {
	for _, outcome := range []bool{false, true} {
		var gate SeedGate
		ctx, cancel := context.WithCancel(t.Context())
		gate.Run(ctx, "host", func(context.Context) bool {
			cancel()
			return outcome
		})
		if _, checked := gate.LastCheck("host"); checked {
			t.Fatalf("caller cancel with pass=%v must not back off the next call", outcome)
		}
	}
}

func TestSeedGateSingleFlight(t *testing.T) {
	var gate SeedGate
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		gate.Run(t.Context(), "host", func(context.Context) bool {
			close(started)
			<-release
			return true
		})
	})
	<-started
	waited := make(chan struct{})
	go func() {
		gate.Run(t.Context(), "host", func(context.Context) bool {
			t.Error("second caller must wait, not run")
			return false
		})
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("second caller returned before the pass finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	<-waited
}
