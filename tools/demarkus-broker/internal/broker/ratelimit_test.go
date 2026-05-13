package broker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// testRateLimitConfig returns a deliberately tight rate-limit profile so
// integration tests can exhaust the bucket with a handful of requests.
// burst=2 keeps the request count per test small without making the
// limiter so coarse that timing-dependent assertions become flaky.
func testRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Tokens: RateLimitRouteConfig{PerMinute: 60, Burst: 2},
		Login:  RateLimitRouteConfig{PerMinute: 60, Burst: 2},
	}
}

func TestRateLimitRegistryNilWhenDisabledOrZero(t *testing.T) {
	// newRateLimitRegistry must return nil for invalid inputs so the
	// "disabled = no enforcement" guarantee in the middleware (nil
	// registry → passthrough) cannot be silently broken by a config
	// that left fields at zero. Pins the contract Routes() depends on.
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
		allowed, _ := reg.reserve("k")
		if !allowed {
			t.Errorf("attempt %d denied; burst=2 should allow first two", i+1)
		}
	}
	allowed, retryAfter := reg.reserve("k")
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
	reg.reserve("k1")
	reg.reserve("k1")
	if allowed, _ := reg.reserve("k1"); allowed {
		t.Fatal("k1 not exhausted after burst+1 attempts")
	}
	// k2's bucket is independent and must still allow.
	if allowed, _ := reg.reserve("k2"); !allowed {
		t.Error("k2 denied even though k1 was the one exhausted — cross-key isolation broken")
	}
}

func TestRateLimitRegistryDenialDoesNotConsumeBudget(t *testing.T) {
	// PerMinute=600 = 10 tokens/sec = 1 token per 100ms. Burst=2.
	// Spam many denials in a tight loop; if denials consumed budget
	// (Reserve without Cancel), each one would push the next-allowed
	// time further out, and 150ms wouldn't be enough to recover.
	// With the Reserve+Cancel pattern, one token regenerates per
	// 100ms regardless of denial volume.
	reg := newRateLimitRegistry(600, 2)
	reg.reserve("k")
	reg.reserve("k")
	for i := range 50 {
		if allowed, _ := reg.reserve("k"); allowed {
			t.Fatalf("attempt %d unexpectedly allowed before regen window", i)
		}
	}
	time.Sleep(150 * time.Millisecond)
	if allowed, _ := reg.reserve("k"); !allowed {
		t.Error("recovery after 150ms denied — denials consumed budget (Cancel missing)")
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

func TestSubjectRateLimitMissingClaimsIs500(t *testing.T) {
	// Regression guard: if a future route registration composes
	// subjectRateLimit without requireAuth upstream, the request
	// reaches the middleware with no claims in ctx. We surface that
	// as 500 + an error log rather than silently disabling the limit
	// (which would be the worst-of-both: production looks fine, but
	// a misbehaving caller gets unbounded throughput).
	srv := NewServer(testConfigWithRateLimit(), newTestSigner(t), &fakeVerifier{}, NewIssuer(testConfig(), fake.NewSimpleClientset()), nil)
	h := srv.subjectRateLimit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler reached despite missing claims")
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/tokens", http.NoBody)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// testConfigWithRateLimit returns a testConfig with a tight rate limit
// applied, plus enough multi-claim verifier wiring usable across the
// subject-keyed tests below. Splitting it out keeps each test focused
// on the property it pins.
func testConfigWithRateLimit() *Config {
	cfg := testConfig()
	cfg.RateLimit = testRateLimitConfig()
	return cfg
}

// twoSubjectVerifier returns a fakeVerifier whose VerifyIDToken maps
// bearer string → distinct Claims (different Subject = different
// hashSubject = different rate-limit key). Used to pin cross-subject
// isolation: alice exhausts her bucket, bob is unaffected.
func twoSubjectVerifier() *fakeVerifier {
	return &fakeVerifier{
		verifyFn: func(raw string) (Claims, error) {
			switch raw {
			case "alice-token":
				return Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|alice"}, nil
			case "bob-token":
				return Claims{Email: "bob@example.com", EmailVerified: true, Subject: "google|bob"}, nil
			default:
				return Claims{}, errors.New("unknown bearer token")
			}
		},
	}
}

func TestRateLimitTokensExhaustsAndReturns429WithRetryAfter(t *testing.T) {
	cfg := testConfigWithRateLimit()
	srv, _ := newTestServer(t, cfg, twoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	// burst=2, so the first two requests pass and the third 429s.
	for i := range 2 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
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
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
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
	srv, _ := newTestServer(t, cfg, twoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	for i := range 3 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
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
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
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

func TestRateLimitTokensSharedBucketAcrossRoutes(t *testing.T) {
	// Plan §6.2 C.4 decision: the three /tokens routes share one
	// per-subject bucket so a misbehaving client cannot multiply
	// effective throughput by fanning out (list + revoke + rotate
	// each at 10/min would give 30/min effective). With burst=2,
	// 1 list + 1 delete should be allowed; the 3rd request — on a
	// distinct route — must 429.
	cfg := testConfigWithRateLimit()
	srv, _ := newTestServer(t, cfg, twoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	// 1st: GET /tokens (200, empty list).
	req1, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
	req1.Header.Set("Authorization", "Bearer alice-token")
	r1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("1st (GET): %v", err)
	}
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("1st (GET) status = %d, want 200", r1.StatusCode)
	}
	// 2nd: DELETE /tokens/nope (404 from handler, but counts).
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/tokens/usr_nope", http.NoBody)
	req2.Header.Set("Authorization", "Bearer alice-token")
	r2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("2nd (DELETE): %v", err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Fatalf("2nd (DELETE) status = %d, want 404 (handler-side; rate limit must not fire here)", r2.StatusCode)
	}
	// 3rd: POST /tokens/.../rotate on a different route — must 429
	// because the shared bucket is now empty.
	req3, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/usr_nope/rotate", http.NoBody)
	req3.Header.Set("Authorization", "Bearer alice-token")
	r3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("3rd (ROTATE): %v", err)
	}
	_ = r3.Body.Close()
	if r3.StatusCode != http.StatusTooManyRequests {
		t.Errorf("3rd (ROTATE on a different route) status = %d, want 429 — shared-bucket invariant broken", r3.StatusCode)
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
	srv, _ := newTestServer(t, cfg, &fakeVerifier{authURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
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
	srv, _ := newTestServer(t, cfg, &fakeVerifier{authURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
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
	srv, _ := newTestServer(t, cfg, &fakeVerifier{authURL: "https://idp.example.com/authorize"}, fake.NewSimpleClientset())
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
	cfg := testConfig()
	cfg.RateLimit = RateLimitConfig{Disabled: true}
	srv, _ := newTestServer(t, cfg, twoSubjectVerifier(), fake.NewSimpleClientset())
	client := testClient(srv)

	for i := range 20 {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
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
