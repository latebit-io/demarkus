package core

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestSubjectRateLimitMissingClaimsIs500(t *testing.T) {
	// A route composed without the auth middleware upstream reaches the
	// limiter with no claims; that is a 500 and a log line, never a bypass.
	h := SubjectRateLimit(newRateLimitRegistry(60, 2), slog.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler reached despite missing claims")
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/me/install", http.NoBody)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}
