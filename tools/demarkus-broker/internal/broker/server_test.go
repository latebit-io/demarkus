package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// testHTTPTimeout caps every test request against the in-process broker so
// a deadlocked handler fails the offending test in seconds instead of
// hanging until Go's overall test timeout (10m by default) and obscuring
// which case actually broke. Five seconds is generous — every handler is
// purely in-memory and should complete in microseconds.
const testHTTPTimeout = 5 * time.Second

// testClient returns an HTTP client that trusts the test server's
// self-signed certificate and has the test timeout applied. We work on a
// shallow copy because httptest.Server.Client() returns the same instance
// across calls; per-test mutations to Jar / CheckRedirect would otherwise
// leak between tests.
func testClient(srv *httptest.Server) *http.Client {
	base := srv.Client()
	c := *base
	c.Timeout = testHTTPTimeout
	return &c
}

func newTestServer(t *testing.T, cfg *Config, verifier Verifier, k8s *fake.Clientset) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	signer := newTestSigner(t)
	// Default: no id-token signer wired. Refresh-grant tests use
	// newTestServerWithSigner so existing tests don't acquire an
	// extra (unobservable) construction cost they don't care about.
	// Leave the server's clock at the default time.Now — pinning it to
	// a fixed date would expire the OIDC state cookie under any real
	// wall-clock that is later than that date, breaking the callback
	// tests.
	brokerSrv = NewServer(cfg, signer, verifier, NewK8sSecretStore(k8s), nil, nil, nil)
	// NewTLSServer (not NewServer): the state cookie is set with
	// Secure=true and Path=/auth/callback, attributes only enforceable
	// over HTTPS. Without TLS we'd be relying on manual AddCookie calls
	// in tests, which bypass jar policy and mask regressions where the
	// cookie scope was accidentally widened.
	testSrv = httptest.NewTLSServer(brokerSrv.Routes())
	t.Cleanup(testSrv.Close)
	return testSrv, brokerSrv
}

// newTestServerWithSigner is the test helper for refresh-grant /
// JWKS / broker-signed verification tests. Identical shape to
// newTestServer but wires an ephemeral IDTokenSigner so /device/token
// refresh, /.well-known/jwks.json, and the composite-verifier
// broker-leg are all live.
func newTestServerWithSigner(t *testing.T, cfg *Config, verifier Verifier, k8s *fake.Clientset, signer *IDTokenSigner) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	cookieSigner := newTestSigner(t)
	brokerSrv = NewServer(cfg, cookieSigner, verifier, NewK8sSecretStore(k8s), nil, signer, nil)
	testSrv = httptest.NewTLSServer(brokerSrv.Routes())
	t.Cleanup(testSrv.Close)
	return testSrv, brokerSrv
}

func TestHealthAndReady(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := testClient(srv).Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
	}
}

// TestNewServerPanicsOnDiscoveryWithoutSigner pins the construction
// invariant added in PR4 review: a Discovery doc that advertises
// jwks_uri at a route the broker doesn't serve is broken-by-
// construction. NewServer panics rather than silently produce a
// half-wired surface.
func TestNewServerPanicsOnDiscoveryWithoutSigner(t *testing.T) {
	idpMux := http.NewServeMux()
	idpMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"https://idp.example.com"}`)
	})
	idp := httptest.NewServer(idpMux)
	t.Cleanup(idp.Close)
	d, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.URL,
	})
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}
	cfg := testConfig()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for discovery != nil + idTokenSigner == nil")
		}
	}()
	NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewK8sSecretStore(fake.NewSimpleClientset()), d, nil, nil)
}

// TestWellKnownDiscoveryRouteRegistered guards Routes() composition: with
// Discovery wired, the mux serves openid-configuration AND the RFC 8414
// alias (strict clients need the latter on the issuer origin — this listener).
func TestWellKnownDiscoveryRouteRegistered(t *testing.T) {
	idpMux := http.NewServeMux()
	idpMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"https://idp.example.com","jwks_uri":"https://idp.example.com/jwks"}`)
	})
	idp := httptest.NewServer(idpMux)
	t.Cleanup(idp.Close)

	d, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.URL,
	})
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}
	cfg := testConfig()
	// PR4 invariant: discovery != nil requires a non-nil
	// IDTokenSigner so the jwks_uri override the discovery doc
	// advertises actually has a handler mounted. NewServer panics
	// otherwise.
	srv := NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewK8sSecretStore(fake.NewSimpleClientset()), d, newTestIDTokenSigner(t), nil)
	tsrv := httptest.NewServer(srv.Routes())
	t.Cleanup(tsrv.Close)

	for _, path := range []string{"/.well-known/openid-configuration", "/.well-known/oauth-authorization-server"} {
		resp, err := testClient(tsrv).Get(tsrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s body: %v", path, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if got, _ := doc["issuer"].(string); got != "https://broker.example.com" {
			t.Errorf("%s issuer = %q, want broker URL (override applied via mounted route)", path, got)
		}
	}
}

// TestWellKnownDiscoveryRouteSkippedWhenNil guards the inverse: tests
// that don't construct a Discovery (the common case for server_test.go)
// get a 404 rather than a nil dereference. Without the nil-guard in
// Routes(), the bare NewServer(..., nil, ...) call in newTestServer
// would crash any handler test that happened to hit /.well-known.
func TestWellKnownDiscoveryRouteSkippedWhenNil(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	resp, err := testClient(srv).Get(srv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET well-known: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when discovery is nil", resp.StatusCode)
	}
}

func TestAuthLoginRedirectsAndSetsStateCookie(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example.com/authorize") {
		t.Errorf("Location = %q", loc)
	}
	var stateCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("state cookie not set")
	}
	if !stateCookie.HttpOnly || !stateCookie.Secure {
		t.Errorf("cookie attrs HttpOnly=%v Secure=%v", stateCookie.HttpOnly, stateCookie.Secure)
	}
}

// loginAndExtract simulates the /auth/login leg of the OIDC flow and
// returns a client whose cookie jar holds the state cookie, plus the
// nonce from the IdP redirect URL. Callers issue the /auth/callback
// request through the returned client so the jar (not a manual
// req.AddCookie call) replays the state cookie — that's the only way to
// exercise the Secure=true + Path=/auth/callback attributes for real.
func loginAndExtract(t *testing.T, srv *httptest.Server) (client *http.Client, nonce string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client = testClient(srv)
	client.Jar = jar
	// Stop at the 302 to the IdP — we synthesize the callback ourselves
	// rather than chasing the redirect off-cluster.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	nonce = loc.Query().Get("state")
	if nonce == "" {
		t.Fatalf("missing state nonce after /auth/login (Location=%q)", resp.Header.Get("Location"))
	}
	// Sanity: the jar must have captured the state cookie from the
	// 302. If it didn't, attributes like Path or Secure are wrong for
	// the test's HTTPS server.
	u, _ := url.Parse(srv.URL + "/auth/callback")
	var has bool
	for _, c := range jar.Cookies(u) {
		if c.Name == stateCookieName {
			has = true
			break
		}
	}
	if !has {
		t.Fatalf("state cookie not retained by jar — check Secure/Path attributes")
	}
	return client, nonce
}

// TestStateCookieSecureMatchesInsecureCookiesFlag locks BOTH cookie-writing
// sites (the set on /auth/login and the clear on /auth/callback) against
// the same flag, so a future change that flips only one is caught here.
func TestStateCookieSecureMatchesInsecureCookiesFlag(t *testing.T) {
	tests := []struct {
		name            string
		insecureCookies bool
		wantSecure      bool
	}{
		{"default keeps Secure=true on set and clear", false, true},
		{"InsecureCookies drops Secure on set and clear", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Server.InsecureCookies = tt.insecureCookies
			verifier := &fakeVerifier{
				authURL: "https://idp.example.com/authorize",
				claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|123"},
			}
			srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

			// Single /auth/login so the jar captures one state cookie
			// whose nonce we can replay on /auth/callback. A second
			// /auth/login would rotate the nonce and the callback would
			// fail at the state-mismatch guard before reaching the
			// clear-cookie write.
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			client := testClient(srv)
			client.Jar = jar
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			loginResp, err := client.Get(srv.URL + "/auth/login")
			if err != nil {
				t.Fatalf("GET /auth/login: %v", err)
			}
			_ = loginResp.Body.Close()
			setCookie := findStateCookie(loginResp.Cookies())
			if setCookie == nil {
				t.Fatal("state cookie not set on /auth/login")
			}
			if setCookie.Secure != tt.wantSecure {
				t.Errorf("login Secure = %v, want %v", setCookie.Secure, tt.wantSecure)
			}
			loc, err := url.Parse(loginResp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse redirect Location: %v", err)
			}
			nonce := loc.Query().Get("state")
			if nonce == "" {
				t.Fatalf("missing state nonce after /auth/login")
			}

			// /auth/callback emits a clearing Set-Cookie with MaxAge<0
			// using the same Secure flag. Verifying it here is the
			// load-bearing assertion: without this, a regression on the
			// clear path would slip past the login-path test.
			req, _ := http.NewRequest(http.MethodGet,
				srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
			cbResp, err := client.Do(req)
			if err != nil {
				t.Fatalf("callback: %v", err)
			}
			_ = cbResp.Body.Close()
			if cbResp.StatusCode != http.StatusOK {
				t.Fatalf("callback status = %d, want 200 (so clear-cookie write is reached)", cbResp.StatusCode)
			}
			clearCookie := findStateCookie(cbResp.Cookies())
			if clearCookie == nil {
				t.Fatal("state cookie not cleared on /auth/callback")
			}
			if clearCookie.MaxAge >= 0 {
				t.Errorf("clear MaxAge = %d, want < 0 (cookie should be cleared)", clearCookie.MaxAge)
			}
			if clearCookie.Secure != tt.wantSecure {
				t.Errorf("callback clear Secure = %v, want %v", clearCookie.Secure, tt.wantSecure)
			}
		})
	}
}

func findStateCookie(cookies []*http.Cookie) *http.Cookie {
	for _, c := range cookies {
		if c.Name == stateCookieName {
			return c
		}
	}
	return nil
}

func TestAuthCallbackSuccess(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|123"},
	}
	cfg := testConfig()
	// PublicURL is required for the callback to include the world in
	// its response — see the parity-with-/me/install filter in
	// server.go's authCallback. testConfig's default is "" so we set
	// it explicitly here.
	cfg.Worlds[0].PublicURL = "mark://team-a.cluster.local:6309"
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())
	client, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	// The bare-code branch returns identity + authorized worlds — no
	// raw tokens. Per-world demarkus tokens are minted lazily inside
	// the broker's MCP gateway on first dispatch.
	var got installResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Email != "alice@example.com" {
		t.Errorf("Email = %q, want alice@example.com", got.Email)
	}
	if len(got.Worlds) != 1 {
		t.Fatalf("worlds = %+v, want exactly 1", got.Worlds)
	}
	if got.Worlds[0].Name != "team-a" {
		t.Errorf("Name = %q, want team-a", got.Worlds[0].Name)
	}
	// Parity guard with /me/install: the callback must filter
	// PublicURL-less worlds and emit the same no-store cache
	// headers. Without these assertions either contract can drift
	// silently on the callback branch.
	if got.Worlds[0].PublicURL == "" {
		t.Errorf("Worlds[0].PublicURL empty; non-installable worlds must be filtered")
	}
	if h := resp.Header.Get("Cache-Control"); h != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", h)
	}
	if h := resp.Header.Get("Pragma"); h != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", h)
	}
}

func TestAuthCallbackStateMismatch(t *testing.T) {
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	client, _ := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?code=abc&state=wrong-nonce", http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthCallbackMissingCookie(t *testing.T) {
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	resp, err := testClient(srv).Get(srv.URL + "/auth/callback?code=abc&state=irrelevant")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAuthCallbackExchangeFails(t *testing.T) {
	verifier := &fakeVerifier{exchErr: errors.New("idp said no")}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	client, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthCallbackUnauthorizedDomain(t *testing.T) {
	verifier := &fakeVerifier{claims: Claims{Email: "mallory@evil.example", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	client, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// Sanity guard so the clock override on Server is exercised at least once.
func TestServerClockExposed(t *testing.T) {
	cfg := testConfig()
	s := NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewK8sSecretStore(fake.NewSimpleClientset()), nil, nil, nil)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return fixed }
	if !s.clock().Equal(fixed) {
		t.Error("clock override not honored")
	}
}
