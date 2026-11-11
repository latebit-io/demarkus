package httpauth

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// subjectRegistry is a subject limiter of perMinute and burst.
func subjectRegistry(perMinute, burst int) *core.RateLimitRegistry {
	subject, _ := core.NewRateLimits(&core.RateLimitConfig{Tokens: core.RateLimitRouteConfig{PerMinute: perMinute, Burst: burst}})
	return subject
}

func TestBearerToken(t *testing.T) {
	for header, want := range map[string]string{
		"":              "",
		"Bearer abc":    "abc",
		"bearer abc":    "abc",
		"BEARER abc":    "abc",
		"Basic abc":     "",
		"Bearer":        "",
		"Bearer a b":    "",
		"  Bearer abc ": "abc",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := BearerToken(r); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSubjectRateLimitMissingClaimsIs500(t *testing.T) {
	// A route composed without the auth middleware upstream reaches the
	// limiter with no claims; that is a 500 and a log line, never a bypass.
	h := SubjectRateLimit(subjectRegistry(60, 2), slog.Default(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

// A refusal flood is one log line per window, while every request is still
// refused: the limiter must not turn into a log-volume amplifier.
func TestSubjectRateLimitLogsRefusalFloodOnce(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := SubjectRateLimit(subjectRegistry(1, 1), log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx := core.CtxWithClaims(context.Background(), &core.Claims{Subject: "alice"})
	refused := 0
	for range 50 {
		rr := httptest.NewRecorder()
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/mcp", http.NoBody)
		h.ServeHTTP(rr, r)
		if rr.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused != 49 {
		t.Fatalf("refused %d of 50, want 49", refused)
	}
	if got := strings.Count(logs.String(), "rate limit exceeded"); got != 1 {
		t.Fatalf("logged %d refusal lines, want 1:\n%s", got, logs.String())
	}
}
