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
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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
	issuer := newIssuer(t, cfg, k8s)
	// Default: no id-token signer wired. Refresh-grant tests use
	// newTestServerWithSigner so existing tests don't acquire an
	// extra (unobservable) construction cost they don't care about.
	// Leave the server's clock at the default time.Now — pinning it to
	// the issuer's fixed date would expire the OIDC state cookie under
	// any real wall-clock that is later than that date, breaking the
	// callback tests. The issuer's clock stays pinned so token-expiry
	// assertions in issuer tests remain deterministic.
	brokerSrv = NewServer(cfg, signer, verifier, issuer, nil, nil, nil)
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
	issuer := newIssuer(t, cfg, k8s)
	brokerSrv = NewServer(cfg, cookieSigner, verifier, issuer, nil, signer, nil)
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
	NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewIssuer(cfg, fake.NewSimpleClientset()), d, nil, nil)
}

// TestWellKnownDiscoveryRouteRegistered guards the Routes() composition:
// when a Discovery is wired into NewServer, the broker mux exposes it at
// /.well-known/openid-configuration. The discovery_test.go suite covers
// the proxy + cache mechanics; this test just confirms the mount point so
// a future refactor can't quietly drop the route registration.
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
	srv := NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewIssuer(cfg, fake.NewSimpleClientset()), d, newTestIDTokenSigner(t), nil)
	tsrv := httptest.NewServer(srv.Routes())
	t.Cleanup(tsrv.Close)

	resp, err := testClient(tsrv).Get(tsrv.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("GET well-known: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := doc["issuer"].(string); got != "https://broker.example.com" {
		t.Errorf("issuer = %q, want broker URL (override applied via mounted route)", got)
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
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
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
	var got struct {
		Tokens []MintResult `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Count is a precondition for the field-level assertions below;
	// without it the indexing on Line 0 would panic instead of giving a
	// useful diff.
	if len(got.Tokens) != 1 {
		t.Fatalf("tokens = %+v, want exactly 1", got.Tokens)
	}
	if got.Tokens[0].World != "team-a" {
		t.Errorf("world = %q, want team-a", got.Tokens[0].World)
	}
	if got.Tokens[0].RawToken == "" {
		t.Error("RawToken empty in callback response")
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

func TestListTokens(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, k8s)
	// Use the issuer directly to seed two issuances for alice; bypasses
	// the OIDC flow, which is exercised in the callback tests above.
	i := newIssuer(t, testConfig(), k8s)
	if _, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("seed mint #1: %v", err)
	}
	if _, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("seed mint #2: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
	req.Header.Set("Authorization", "Bearer some-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var got listTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Tokens) != 2 {
		t.Errorf("got %d issuances, want 2: %+v", len(got.Tokens), got.Tokens)
	}
	for _, iss := range got.Tokens {
		if iss.Email != "alice@example.com" {
			t.Errorf("foreign issuance leaked: %+v", iss)
		}
	}
}

func TestListTokensUnauthenticated(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	resp, err := testClient(srv).Get(srv.URL + "/tokens")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestListTokensInvalidBearer(t *testing.T) {
	verifier := &fakeVerifier{
		verifyFn: func(_ string) (Claims, error) { return Claims{}, errors.New("bad sig") },
	}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/tokens", http.NoBody)
	req.Header.Set("Authorization", "Bearer not-actually-a-jwt")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestDeleteToken(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, k8s)
	i := newIssuer(t, testConfig(), k8s)
	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed mint: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("seed mint returned no results")
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/tokens/"+results[0].Label, http.NoBody)
	req.Header.Set("Authorization", "Bearer some-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	list, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 issuances after delete, got %d", len(list))
	}
}

func TestDeleteTokenNotOwner(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	cfg := testConfig()
	i := newIssuer(t, cfg, k8s)
	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("seed mint returned no results")
	}

	// Verifier returns mallory's identity for the bearer token, so the
	// owner check inside Revoke should fail.
	verifier := &fakeVerifier{claims: Claims{Email: "mallory@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/tokens/"+results[0].Label, http.NoBody)
	req.Header.Set("Authorization", "Bearer mallory-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestDeleteTokenNotFound(t *testing.T) {
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/tokens/usr_nope", http.NoBody)
	req.Header.Set("Authorization", "Bearer some-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRotateTokenSuccess(t *testing.T) {
	// Bearer-authed POST /tokens/{label}/rotate returns 200 with the
	// new MintResult JSON. Alice mints, then rotates; verifier is
	// configured to return alice's identity on bearer verify so the
	// owner check passes.
	k8s := fake.NewSimpleClientset()
	cfg := testConfig()
	i := newIssuer(t, cfg, k8s)
	minted, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed mint: %v", err)
	}
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/"+minted[0].Label+"/rotate", http.NoBody)
	req.Header.Set("Authorization", "Bearer alice-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	var got MintResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Label == "" || got.Label == minted[0].Label {
		t.Errorf("new label = %q, want non-empty and != old %q", got.Label, minted[0].Label)
	}
	if got.RawToken == "" {
		t.Error("RawToken empty in rotate response")
	}
	if got.World != "team-a" {
		t.Errorf("world = %q, want team-a", got.World)
	}
}

func TestRotateTokenNotOwner(t *testing.T) {
	// Alice mints, mallory tries to rotate. Verifier returns mallory's
	// claims for the bearer; owner check inside RotateLabel fails →
	// 403 (matches the DELETE owner-check shape from Slice B).
	k8s := fake.NewSimpleClientset()
	cfg := testConfig()
	i := newIssuer(t, cfg, k8s)
	minted, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed mint: %v", err)
	}
	verifier := &fakeVerifier{claims: Claims{Email: "mallory@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/"+minted[0].Label+"/rotate", http.NoBody)
	req.Header.Set("Authorization", "Bearer mallory-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRotateTokenNoLongerAuthorized(t *testing.T) {
	// Operator tightened the Allow predicate between mint and rotate
	// to a domain alice no longer matches. Bearer is still valid
	// (IdP didn't disable her account) but the world's allowlist
	// rejects → 403 with the "no longer authorized" message. This
	// is the rotate-as-relogin half of the re-auth story; without
	// this gate a user removed from a group could rotate forever.
	k8s := fake.NewSimpleClientset()
	cfg := testConfig()
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"example.com"}}
	i := newIssuer(t, cfg, k8s)
	minted, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed mint: %v", err)
	}
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"other.example"}}

	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, cfg, verifier, k8s)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/"+minted[0].Label+"/rotate", http.NoBody)
	req.Header.Set("Authorization", "Bearer alice-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRotateTokenNotFound(t *testing.T) {
	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/usr_nope/rotate", http.NoBody)
	req.Header.Set("Authorization", "Bearer some-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestRotateTokenSoftPartial(t *testing.T) {
	// The handler's "mint succeeded, old revoke failed" branch is
	// the most consequential code path on the rotate response —
	// callers see 200 with a fresh token but the old label is still
	// live in both Secrets until the sweeper retires it on expiry.
	// Reactor strategy: count Updates against team-a's tokens
	// Secret. The mint phase writes the new label as Update #1 and
	// must pass. The revoke phase writes the old-label removal as
	// Update #2 and we fail it with ServiceUnavailable (mutateSecret
	// only retries on Conflict, so the unavailable propagates).
	// Result: the new label is committed, the old label is NOT
	// removed, and the handler returns 200 because the user's
	// rotation succeeded from their perspective.
	k8s := fake.NewSimpleClientset()
	cfg := testConfig()
	i := newIssuer(t, cfg, k8s)
	minted, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed mint: %v", err)
	}
	oldLabel := minted[0].Label

	var teamAUpdates atomic.Int32
	k8s.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok || ua.GetNamespace() != "team-a" {
			return false, nil, nil
		}
		n := teamAUpdates.Add(1)
		if n >= 2 {
			// Second team-a update is the revoke phase's removal
			// of the old label. Simulate a transient k8s API blip
			// — mutateSecret only retries on Conflict so this
			// error propagates straight up to revokeIssuance.
			return true, nil, apierrors.NewServiceUnavailable("simulated transient blip during old-label revoke")
		}
		return false, nil, nil
	})

	verifier := &fakeVerifier{claims: Claims{Email: "alice@example.com", EmailVerified: true}}
	srv, _ := newTestServer(t, cfg, verifier, k8s)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/"+oldLabel+"/rotate", http.NoBody)
	req.Header.Set("Authorization", "Bearer alice-id-token")
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s — soft-partial path must still return 200", resp.StatusCode, body)
	}
	var got MintResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Label == "" || got.Label == oldLabel {
		t.Errorf("new label = %q, want non-empty and != old %q", got.Label, oldLabel)
	}
	if got.RawToken == "" {
		t.Error("RawToken empty in soft-partial response")
	}

	// World Secret: BOTH labels still present — the old one
	// because revoke failed, the new one because mint succeeded.
	// The sweeper will retire the old on expiry.
	worldSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world Secret: %v", err)
	}
	gotTOML := string(worldSecret.Data[TokensSecretKey])
	if !strings.Contains(gotTOML, oldLabel) {
		t.Errorf("old label gone from world Secret despite failed revoke:\n%s", gotTOML)
	}
	if !strings.Contains(gotTOML, got.Label) {
		t.Errorf("new label missing from world Secret on soft-partial:\n%s", gotTOML)
	}

	// Issuances Secret: both entries too. Owner alice now has TWO
	// live issuances; this is the transient state the sweeper
	// resolves on expiry.
	live, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 2 {
		t.Errorf("issuances after soft-partial rotate = %d, want 2 (old + new live until sweep)", len(live))
	}
}

func TestRotateTokenUnauthenticated(t *testing.T) {
	// No bearer at all → 401 from the shared authenticate helper,
	// before RotateLabel even runs. Same shape as the equivalent
	// DELETE and GET /tokens unauthenticated guards.
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/tokens/whatever/rotate", http.NoBody)
	resp, err := testClient(srv).Do(req)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Sanity guard so the clock override on Server is exercised at least once.
func TestServerClockExposed(t *testing.T) {
	cfg := testConfig()
	s := NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewIssuer(cfg, fake.NewSimpleClientset()), nil, nil, nil)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return fixed }
	if !s.clock().Equal(fixed) {
		t.Error("clock override not honored")
	}
}
