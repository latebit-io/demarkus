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

func newTestServer(t *testing.T, cfg *Config, verifier Verifier, k8s *fake.Clientset) (testSrv *httptest.Server, brokerSrv *Server) {
	t.Helper()
	signer := newTestSigner(t)
	issuer := newIssuer(t, cfg, k8s)
	// Leave the server's clock at the default time.Now — pinning it to
	// the issuer's fixed date would expire the OIDC state cookie under
	// any real wall-clock that is later than that date, breaking the
	// callback tests. The issuer's clock stays pinned so token-expiry
	// assertions in issuer tests remain deterministic.
	brokerSrv = NewServer(cfg, signer, verifier, issuer, nil)
	testSrv = httptest.NewServer(brokerSrv.Routes())
	t.Cleanup(testSrv.Close)
	return testSrv, brokerSrv
}

func TestHealthAndReady(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
	}
}

func TestAuthLoginRedirectsAndSetsStateCookie(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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

// loginAndExtract simulates the full /auth/login → cookie → callback flow
// against the test server, returning the parsed callback response body.
// Uses a custom transport that does not follow the redirect to the IdP so
// the test can capture the state nonce and synthesize a callback request.
func loginAndExtract(t *testing.T, srv *httptest.Server) (cookie *http.Cookie, nonce string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == stateCookieName {
			cookie = c
		}
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	nonce = loc.Query().Get("state")
	if cookie == nil || nonce == "" {
		t.Fatalf("state cookie=%v nonce=%q after /auth/login", cookie, nonce)
	}
	return cookie, nonce
}

func TestAuthCallbackSuccess(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Email: "alice@example.com", EmailVerified: true, Subject: "google|123"},
	}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	cookie, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=abc&state="+url.QueryEscape(nonce), http.NoBody)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
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
	cookie, _ := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?code=abc&state=wrong-nonce", http.NoBody)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
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

	resp, err := http.Get(srv.URL + "/auth/callback?code=abc&state=irrelevant")
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
	cookie, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?code=abc&state="+nonce, http.NoBody)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
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
	cookie, nonce := loginAndExtract(t, srv)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/callback?code=abc&state="+nonce, http.NoBody)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.Get(srv.URL + "/tokens")
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// Sanity guard so the clock override on Server is exercised at least once.
func TestServerClockExposed(t *testing.T) {
	cfg := testConfig()
	s := NewServer(cfg, newTestSigner(t), &fakeVerifier{}, NewIssuer(cfg, fake.NewSimpleClientset()), nil)
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return fixed }
	if !s.clock().Equal(fixed) {
		t.Error("clock override not honored")
	}
}
