package oauthsrv

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"k8s.io/client-go/kubernetes/fake"
)

// testRateLimitConfig returns a deliberately tight rate-limit profile so
// integration tests can exhaust the bucket with a handful of requests.
// burst=2 keeps the request count per test small without making the
// limiter so coarse that timing-dependent assertions become flaky.
func testRateLimitConfig() core.RateLimitConfig {
	return core.RateLimitConfig{
		Tokens: core.RateLimitRouteConfig{PerMinute: 60, Burst: 2},
		Login:  core.RateLimitRouteConfig{PerMinute: 60, Burst: 2},
	}
}

func TestServerClientIP(t *testing.T) {
	// clientIP gates the IP-keyed limiter; getting this wrong either
	// makes the limiter useless behind an Ingress (every request
	// appears as one IP) or trivially bypassable via XFF header
	// spoofing. Table covers both regression vectors.
	tests := []struct {
		name              string
		trustForwardedFor bool
		remoteAddr        string
		xff               string
		want              string
	}{
		{
			name:       "untrusted XFF ignored",
			remoteAddr: "10.0.0.5:55555",
			xff:        "1.2.3.4",
			want:       "10.0.0.5",
		},
		{
			name:              "trusted XFF single hop",
			trustForwardedFor: true,
			remoteAddr:        "10.0.0.5:55555",
			xff:               "1.2.3.4",
			want:              "1.2.3.4",
		},
		{
			name:              "trusted XFF multi hop takes leftmost",
			trustForwardedFor: true,
			remoteAddr:        "10.0.0.5:55555",
			xff:               "1.2.3.4, 10.0.0.1, 10.0.0.2",
			want:              "1.2.3.4",
		},
		{
			name:              "trusted XFF empty falls back to RemoteAddr",
			trustForwardedFor: true,
			remoteAddr:        "10.0.0.5:55555",
			xff:               "",
			want:              "10.0.0.5",
		},
		{
			name:              "trusted XFF with only whitespace falls back",
			trustForwardedFor: true,
			remoteAddr:        "10.0.0.5:55555",
			xff:               " , ",
			want:              "10.0.0.5",
		},
		{
			name:       "ipv6 RemoteAddr",
			remoteAddr: "[::1]:55555",
			want:       "::1",
		},
		{
			name:       "invalid RemoteAddr falls back to raw",
			remoteAddr: "garbage",
			want:       "garbage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{trustForwardedFor: tt.trustForwardedFor}
			r, _ := http.NewRequest(http.MethodGet, "/", http.NoBody)
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := s.clientIP(r); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

// testConfigWithRateLimit is the base fixture with the tight limits.
func testConfigWithRateLimit() *core.Config {
	cfg := brokertest.NewConfig()
	cfg.RateLimit = testRateLimitConfig()
	return cfg
}

func TestRateLimitTokensExhaustsAndReturns429WithRetryAfter(t *testing.T) {
	cfg := testConfigWithRateLimit()
	srv, _ := newTestServer(t, cfg, brokertest.TwoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	// burst=2, so the first two requests pass and the third 429s.
	for i := range 2 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
		req.Header.Set("Authorization", "Bearer alice-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d status = %d, want 200 (within burst)", i+1, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
	req.Header.Set("Authorization", "Bearer alice-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("3rd: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("3rd status = %d body=%s, want 429", resp.StatusCode, body)
	}
	// Retry-After header must be set with a positive integer-second
	// hint so CLI clients can honor it. A missing or 0 value would
	// be read as "retry now" and defeat the limiter.
	if h := resp.Header.Get("Retry-After"); h == "" || h == "0" {
		t.Errorf("Retry-After = %q, want a positive integer-second hint", h)
	}
}

func TestRateLimitTokensCrossSubjectIsolation(t *testing.T) {
	// Alice exhausts her bucket; bob's identity has a different
	// Subject → different hashSubject → different bucket → his
	// requests still pass. Without this property, a single noisy
	// user could DoS the entire authed surface for everyone else.
	cfg := testConfigWithRateLimit()
	srv, _ := newTestServer(t, cfg, brokertest.TwoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	for i := range 3 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
		req.Header.Set("Authorization", "Bearer alice-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("alice attempt %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if i < 2 && resp.StatusCode != http.StatusOK {
			t.Fatalf("alice attempt %d status = %d, want 200", i+1, resp.StatusCode)
		}
		if i == 2 && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("alice attempt 3 status = %d, want 429 (precondition)", resp.StatusCode)
		}
	}
	// Bob should still pass through cleanly.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
	req.Header.Set("Authorization", "Bearer bob-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bob status = %d, want 200 (his bucket should be independent)", resp.StatusCode)
	}
}

func TestRateLimitLoginIPExhaustsAndReturns429WithRetryAfter(t *testing.T) {
	// /auth/login is keyed by source IP. Behind an Ingress every
	// request has the same r.RemoteAddr (the controller IP), so we
	// enable trustForwardedFor and craft per-request XFF values to
	// simulate different originating clients. This test pins the
	// "one IP exhausting its bucket returns 429" property.
	cfg := testConfigWithRateLimit()
	cfg.RateLimit.TrustForwardedFor = true
	srv, _ := newTestServer(t, cfg, &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for i := range 2 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/login", http.NoBody)
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("attempt %d status = %d, want 302 (within burst)", i+1, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/login", http.NoBody)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("3rd: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("3rd status = %d, want 429", resp.StatusCode)
	}
	if h := resp.Header.Get("Retry-After"); h == "" || h == "0" {
		t.Errorf("Retry-After = %q, want positive", h)
	}
}

func TestRateLimitLoginIPCrossIPIsolation(t *testing.T) {
	// Spoofed-IP A exhausts its bucket; spoofed-IP B still passes.
	// Only meaningful with trustForwardedFor=true; the next test
	// pins the opposite default-trust posture.
	cfg := testConfigWithRateLimit()
	cfg.RateLimit.TrustForwardedFor = true
	srv, _ := newTestServer(t, cfg, &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// IP A: 3 attempts (last 429s).
	for i := range 3 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/login", http.NoBody)
		req.Header.Set("X-Forwarded-For", "198.51.100.10")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("IP A attempt %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
	}
	// IP B: must pass — independent bucket.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/login", http.NoBody)
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("IP B: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("IP B status = %d, want 302 — cross-IP isolation broken", resp.StatusCode)
	}
}

func TestRateLimitLoginIPIgnoresForwardedForByDefault(t *testing.T) {
	// trustForwardedFor=false (default). XFF must be ignored so an
	// attacker cannot bypass the per-IP limit by spoofing the
	// header. Both "IPs" share the same actual r.RemoteAddr
	// (127.0.0.1 from httptest), so the bucket exhausts across
	// requests with different XFF values.
	cfg := testConfigWithRateLimit()
	// Explicitly false; the default is already false but the
	// assertion below depends on this — restating is documentation.
	cfg.RateLimit.TrustForwardedFor = false
	srv, _ := newTestServer(t, cfg, &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for i, ip := range []string{"198.51.100.10", "198.51.100.20", "198.51.100.30"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/login", http.NoBody)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("attempt %d (XFF=%s): %v", i+1, ip, err)
		}
		_ = resp.Body.Close()
		if i < 2 && resp.StatusCode != http.StatusFound {
			t.Fatalf("attempt %d status = %d, want 302 within burst", i+1, resp.StatusCode)
		}
		// 3rd must be 429 because all three requests came from the
		// same r.RemoteAddr (XFF is ignored when trust is off).
		if i == 2 && resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("attempt 3 status = %d, want 429 — spoofed XFF must NOT split buckets when trust is off", resp.StatusCode)
		}
	}
}

func TestRateLimitDisabledBypasses(t *testing.T) {
	// rateLimit.disabled=true skips all enforcement: subjectReg and
	// loginReg are nil, both middlewares pass through. Pins the
	// operator-side opt-out so a single-replica dev deployment can
	// disable the limiter without losing any other behavior.
	cfg := brokertest.NewConfig()
	cfg.RateLimit = core.RateLimitConfig{Disabled: true}
	srv, _ := newTestServer(t, cfg, brokertest.TwoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	for i := range 20 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me/install", http.NoBody)
		req.Header.Set("Authorization", "Bearer alice-token")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d status = %d body=%s; disabled limiter must let all requests through",
				i+1, resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
}
