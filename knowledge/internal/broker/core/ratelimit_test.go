package core

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol/memtest"
)

func TestRateLimitRegistryNilWhenDisabledOrZero(t *testing.T) {
	// Zero or negative fields must yield nil, the middleware's no enforcement
	// value, so a half filled config cannot enable a limiter by accident.
	tests := []struct {
		name       string
		perMinute  int
		burst      int
		wantNonNil bool
	}{
		{"zero perMinute", 0, 5, false},
		{"zero burst", 10, 0, false},
		{"negative perMinute", -1, 5, false},
		{"negative burst", 10, -1, false},
		{"valid", 10, 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newRateLimitRegistry(tt.perMinute, tt.burst)
			if (got != nil) != tt.wantNonNil {
				t.Errorf("got=%v, want non-nil=%v", got, tt.wantNonNil)
			}
		})
	}
}

func TestRateLimitRegistryAllowsBurstThenDenies(t *testing.T) {
	reg := newRateLimitRegistry(60, 2)
	for i := range 2 {
		allowed, _ := reg.Reserve("k")
		if !allowed {
			t.Errorf("attempt %d denied; burst=2 should allow first two", i+1)
		}
	}
	allowed, retryAfter := reg.Reserve("k")
	if allowed {
		t.Error("3rd attempt allowed; burst+1 should deny")
	}
	if retryAfter < time.Second {
		// Minimum 1s floor on retryAfter so the HTTP Retry-After
		// header is never 0 (a 0 hints "try again immediately"
		// which an aggressive client interprets as "spam harder").
		t.Errorf("retryAfter = %s, want >= 1s (floor enforced)", retryAfter)
	}
}

func TestRateLimitRegistryCrossKeyIsolation(t *testing.T) {
	reg := newRateLimitRegistry(60, 2)
	// Exhaust k1.
	reg.Reserve("k1")
	reg.Reserve("k1")
	if allowed, _ := reg.Reserve("k1"); allowed {
		t.Fatal("k1 not exhausted after burst+1 attempts")
	}
	// k2's bucket is independent and must still allow.
	if allowed, _ := reg.Reserve("k2"); !allowed {
		t.Error("k2 denied even though k1 was the one exhausted — cross-key isolation broken")
	}
}

func TestRateLimitRegistryDenialDoesNotConsumeBudget(t *testing.T) {
	// 600 per minute regenerates one token per 100ms. If denials consumed
	// budget, a burst of them would push recovery past the 150ms wait below.
	reg := newRateLimitRegistry(600, 2)
	reg.Reserve("k")
	reg.Reserve("k")
	for i := range 50 {
		if allowed, _ := reg.Reserve("k"); allowed {
			t.Fatalf("attempt %d unexpectedly allowed before regen window", i)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if allowed, _ := reg.Reserve("k"); !allowed {
		t.Error("recovery after 150ms denied — denials consumed budget (Cancel missing)")
	}
}

// An idle limiter has refilled, so dropping it changes no answer: the map
// holds the keys of the last window, not every key ever seen.
func TestRateLimitRegistryEvictsIdleLimiters(t *testing.T) {
	reg := newRateLimitRegistry(60, 2) // refills in 2s
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	for i := range 1000 {
		reg.Reserve(fmt.Sprintf("old-%d", i))
	}
	reg.Reserve("busy")
	reg.Reserve("busy")
	now = now.Add(minSweepInterval)
	if allowed, _ := reg.Reserve("fresh"); !allowed {
		t.Fatal("fresh key denied")
	}
	if got := len(reg.limiters); got != 1 {
		t.Fatalf("%d limiters after the sweep, want 1 (fresh)", got)
	}
	// busy was idle for a full refill, so its fresh bucket is the same bucket.
	if allowed, _ := reg.Reserve("busy"); !allowed {
		t.Fatal("refilled key denied after eviction")
	}
}

// A key still short of a full refill keeps its spent bucket across a sweep.
func TestRateLimitRegistrySweepKeepsSpentBuckets(t *testing.T) {
	reg := newRateLimitRegistry(1, 2) // refills in 2m
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	reg.Reserve("k")
	reg.Reserve("k")
	now = now.Add(minSweepInterval)
	reg.Reserve("other") // sweeps
	reg.Reserve("k")     // the one token a minute regenerated
	if allowed, _ := reg.Reserve("k"); allowed {
		t.Fatal("spent bucket was evicted before it refilled: the limit resets early")
	}
}

// Memory tracks the keys of one window: a second flood of distinct keys
// retains no more than the first.
func TestRateLimitRegistryRetainsOneWindow(t *testing.T) {
	reg := newRateLimitRegistry(60, 2)
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	const keys = 20_000
	flood := func(round int) {
		for i := range keys {
			reg.Reserve(fmt.Sprintf("2001:db8:%x:%x::/64", round, i))
		}
		now = now.Add(minSweepInterval)
	}
	first := memtest.Retained(func() { flood(0) })
	later := memtest.Retained(func() {
		for round := 1; round <= 5; round++ {
			flood(round)
		}
	})
	runtime.KeepAlive(reg)
	if later > first/2 {
		t.Fatalf("five more floods retained %d bytes on top of the first's %d; idle limiters are not evicted", later, first)
	}
}
