package broker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// authorizeQuery returns a fully-populated /oauth/authorize query
// string for a happy-path MCP client request. Tests can copy + tweak
// individual fields to exercise each validation axis.
func authorizeQuery() url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {"client-abc"},
		"redirect_uri":          {"http://127.0.0.1:55408/callback"},
		"code_challenge":        {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, // S256 of "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk" (RFC 7636 example)
		"code_challenge_method": {"S256"},
		"state":                 {"client-state-nonce"},
		"scope":                 {"openid email profile"},
	}
}

// jsonError300 decodes an OAuth-style error JSON 400 body.
type jsonAuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func decodeJSONError(t *testing.T, body io.Reader) jsonAuthError {
	t.Helper()
	var doc jsonAuthError
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return doc
}

func parseLocationQuery(t *testing.T, resp *http.Response) (loc *url.URL, q url.Values) {
	t.Helper()
	raw := resp.Header.Get("Location")
	if raw == "" {
		t.Fatalf("no Location header (status=%d)", resp.StatusCode)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse Location %q: %v", raw, err)
	}
	q = u.Query()
	return u, q
}

// TestOAuthAuthorizePreTrustErrorsReturnJSON locks the pre-redirect-
// trust failure path: any error before redirect_uri is validated
// surfaces as a JSON 400, never as a 302 (which could be exploited
// to bounce the user to an attacker-controlled URL).
func TestOAuthAuthorizePreTrustErrorsReturnJSON(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	tests := []struct {
		name    string
		mutate  func(url.Values)
		wantErr string
	}{
		{
			name:    "missing client_id",
			mutate:  func(q url.Values) { q.Del("client_id") },
			wantErr: "invalid_request",
		},
		{
			name:    "missing redirect_uri",
			mutate:  func(q url.Values) { q.Del("redirect_uri") },
			wantErr: "invalid_request",
		},
		{
			name:    "non-loopback redirect_uri",
			mutate:  func(q url.Values) { q.Set("redirect_uri", "https://attacker.example/steal") },
			wantErr: "invalid_request",
		},
		{
			name:    "https loopback rejected",
			mutate:  func(q url.Values) { q.Set("redirect_uri", "https://127.0.0.1:1234/callback") },
			wantErr: "invalid_request",
		},
		{
			name:    "userinfo redirect_uri rejected",
			mutate:  func(q url.Values) { q.Set("redirect_uri", "http://localhost@evil.example/") },
			wantErr: "invalid_request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeQuery()
			tt.mutate(q)
			resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusBadRequest {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
			}
			if got := resp.Header.Get("Location"); got != "" {
				t.Errorf("set Location header on pre-trust error: %q", got)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			doc := decodeJSONError(t, resp.Body)
			if doc.Error != tt.wantErr {
				t.Errorf("error = %q, want %q", doc.Error, tt.wantErr)
			}
		})
	}
}

// TestOAuthAuthorizePostTrustErrorsRedirect locks the post-redirect-
// trust failure path: after redirect_uri is validated as loopback,
// every subsequent error must surface via a 302 to the client's
// redirect_uri with error + state per RFC 6749 §4.1.2.1.
func TestOAuthAuthorizePostTrustErrorsRedirect(t *testing.T) {
	srv, _ := newTestServer(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	tests := []struct {
		name    string
		mutate  func(url.Values)
		wantErr string
	}{
		{
			name:    "missing response_type",
			mutate:  func(q url.Values) { q.Del("response_type") },
			wantErr: "unsupported_response_type",
		},
		{
			name:    "wrong response_type",
			mutate:  func(q url.Values) { q.Set("response_type", "token") },
			wantErr: "unsupported_response_type",
		},
		{
			name:    "missing code_challenge",
			mutate:  func(q url.Values) { q.Del("code_challenge") },
			wantErr: "invalid_request",
		},
		{
			name:    "missing code_challenge_method",
			mutate:  func(q url.Values) { q.Del("code_challenge_method") },
			wantErr: "invalid_request",
		},
		{
			name:    "plain code_challenge_method rejected",
			mutate:  func(q url.Values) { q.Set("code_challenge_method", "plain") },
			wantErr: "invalid_request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeQuery()
			tt.mutate(q)
			resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusFound {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 302; body=%s", resp.StatusCode, body)
			}
			loc, lq := parseLocationQuery(t, resp)
			if !strings.HasPrefix(loc.String(), "http://127.0.0.1:55408/callback") {
				t.Errorf("redirected to %q, want loopback callback", loc.String())
			}
			if got := lq.Get("error"); got != tt.wantErr {
				t.Errorf("error = %q, want %q", got, tt.wantErr)
			}
			if got := lq.Get("state"); got != "client-state-nonce" {
				t.Errorf("state passthrough mismatch: got %q", got)
			}
		})
	}
}

// TestOAuthAuthorizeHappyPathSetsStateCookieAndRedirectsToIdP locks
// the canonical request: a valid /oauth/authorize 302s to the IdP's
// AuthCodeURL with a fresh nonce, AND sets the signed broker state
// cookie scoped to /auth/callback so the eventual callback can
// reassemble dispatch.
func TestOAuthAuthorizeHappyPathSetsStateCookieAndRedirectsToIdP(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Get(srv.URL + "/oauth/authorize?" + authorizeQuery().Encode())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 302; body=%s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://idp.example.com/authorize") {
		t.Errorf("redirected to %q, want IdP authorize URL", loc)
	}

	var stateCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == stateCookieName {
			stateCookie = c
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("state cookie not set on /oauth/authorize")
	}
	if !stateCookie.HttpOnly || !stateCookie.Secure {
		t.Errorf("cookie attrs HttpOnly=%v Secure=%v", stateCookie.HttpOnly, stateCookie.Secure)
	}
	if stateCookie.Path != "/auth/callback" {
		t.Errorf("cookie path = %q, want /auth/callback", stateCookie.Path)
	}
}

// TestOAuthAuthorizeAcceptsLocalhostAndIPv6Loopback confirms the
// loopback validator accepts the three forms RFC 8252 sanctions.
func TestOAuthAuthorizeAcceptsLocalhostAndIPv6Loopback(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	tests := []struct {
		name, redirectURI string
	}{
		{"127.0.0.1", "http://127.0.0.1:1234/callback"},
		{"localhost", "http://localhost:1234/callback"},
		{"::1", "http://[::1]:1234/callback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := authorizeQuery()
			q.Set("redirect_uri", tt.redirectURI)
			resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusFound {
				t.Fatalf("status = %d, want 302 for %q", resp.StatusCode, tt.redirectURI)
			}
			loc := resp.Header.Get("Location")
			if !strings.HasPrefix(loc, "https://idp.example.com/authorize") {
				t.Errorf("redirected to %q, want IdP authorize URL", loc)
			}
		})
	}
}

// TestOAuthAuthorizeEndToEnd drives the full happy-path flow:
// /oauth/authorize → simulated /auth/callback → MCP-client redirect
// with code+state+iss. Uses a cookie jar so the state cookie set at
// /oauth/authorize is replayed on /auth/callback through the same
// path/scope rules the real browser would enforce.
func TestOAuthAuthorizeEndToEnd(t *testing.T) {
	verifier := &fakeVerifier{
		authURL: "https://idp.example.com/authorize",
		claims:  Claims{Subject: "google|123", Email: "alice@example.com", EmailVerified: true},
	}
	cfg := testConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + authorizeQuery().Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", authResp.StatusCode)
	}
	idpRedirect, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse IdP redirect: %v", err)
	}
	nonce := idpRedirect.Query().Get("state")
	if nonce == "" {
		t.Fatal("missing IdP state nonce")
	}

	// Simulate the IdP coming back with a code. The jar replays the
	// state cookie set on /oauth/authorize.
	cbReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=idp-code-abc&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = cbResp.Body.Close() }()
	if cbResp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback status = %d, want 302; body=%s", cbResp.StatusCode, body)
	}

	clientRedirect, err := url.Parse(cbResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse client redirect: %v", err)
	}
	if clientRedirect.Scheme != "http" || clientRedirect.Host != "127.0.0.1:55408" || clientRedirect.Path != "/callback" {
		t.Errorf("client redirect = %q, want loopback callback", clientRedirect.String())
	}
	q := clientRedirect.Query()
	if q.Get("code") == "" {
		t.Error("missing code on client redirect")
	}
	if got := q.Get("state"); got != "client-state-nonce" {
		t.Errorf("state passthrough mismatch: got %q", got)
	}
	if got := q.Get("iss"); got == "" {
		t.Error("missing iss (RFC 9207 mix-up defense)")
	}
}

// TestOAuthAuthorizeStaleStateCookieRoutesToAuthCodeBranch confirms
// the dispatch order: a State cookie carrying AuthCodeID enters the
// auth-code branch even if it also happened to carry DeviceCode (it
// won't in practice — the two entry points each set only their own
// field — but the dispatch order is the load-bearing invariant).
//
// Implementation: the test only exercises pure AuthCodeID since the
// production code paths never set both simultaneously; including
// the "both fields set" case would assert behavior outside the
// reachable state space and risk codifying the wrong order.
func TestOAuthAuthorizeStaleStateCookieRoutesToAuthCodeBranch(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, testConfig(), verifier, fake.NewSimpleClientset())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := testClient(srv)
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.Get(srv.URL + "/oauth/authorize?" + authorizeQuery().Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc == nil {
		t.Fatal("no Location after /oauth/authorize")
	}
	nonce := loc.Query().Get("state")

	cbReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=x&state="+url.QueryEscape(nonce), http.NoBody)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = cbResp.Body.Close() }()
	// Routes to authCodeCallback (302 back to client redirect),
	// not authCallback's default mint-and-JSON path.
	if cbResp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(cbResp.Body)
		t.Fatalf("callback status = %d, want 302 (auth-code dispatch); body=%s", cbResp.StatusCode, body)
	}
	if loc := cbResp.Header.Get("Location"); !strings.HasPrefix(loc, "http://127.0.0.1:55408/callback") {
		t.Errorf("dispatched to non-auth-code branch: Location=%q", loc)
	}
}

// TestDeviceTokenAuthCodeHappyPath drives the /token leg directly
// against a pre-seeded authCodeStore entry. Avoids re-running the
// full IdP dance — the end-to-end test above covers that — and
// instead focuses on the token-mint surface.
func TestDeviceTokenAuthCodeHappyPath(t *testing.T) {
	verifier := &fakeVerifier{
		claims: Claims{Subject: "google|123", Email: "alice@example.com", EmailVerified: true},
	}
	signer := newTestIDTokenSigner(t)
	srv, broker := newTestServerWithSigner(t, testConfig(), verifier, fake.NewSimpleClientset(), signer)

	// Seed the store directly so this test isolates the /token leg.
	codeVerifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id, err := broker.authCodeStore.Begin(&AuthCodeRequest{
		ClientID:            "client-abc",
		RedirectURI:         "http://127.0.0.1:55408/callback",
		ClientState:         "client-state-nonce",
		Scope:               "openid",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("seed Begin: %v", err)
	}
	exchange := ExchangeResult{
		Claims:      Claims{Subject: "google|123", Email: "alice@example.com", EmailVerified: true},
		RawIDToken:  "idp-id-token-raw",
		AccessToken: "idp-access-token-raw",
	}
	code, _, err := broker.authCodeStore.Bind(id, &exchange)
	if err != nil {
		t.Fatalf("seed Bind: %v", err)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {"client-abc"},
		"redirect_uri":  {"http://127.0.0.1:55408/callback"},
		"code_verifier": {codeVerifier},
	}
	resp, err := testClient(srv).Post(srv.URL+"/device/token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /device/token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var doc deviceTokenSuccess
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.AccessToken == "" {
		t.Error("missing access_token")
	}
	if doc.IDToken == "" {
		t.Error("missing id_token")
	}
	if doc.AccessToken != doc.IDToken {
		t.Error("auth-code path should return access_token == id_token (broker convention)")
	}
	if doc.RefreshToken == "" {
		t.Error("missing refresh_token")
	}
	if doc.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", doc.TokenType)
	}
	if doc.ExpiresIn <= 0 {
		t.Errorf("expires_in = %d, want > 0", doc.ExpiresIn)
	}
}

// TestDeviceTokenAuthCodeErrorMapping locks the wire surface: every
// store-side validation failure collapses to invalid_grant, while
// missing required form params surface as invalid_request. Replay
// of a successful redemption surfaces as invalid_grant.
func TestDeviceTokenAuthCodeErrorMapping(t *testing.T) {
	codeVerifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Each subtest builds a fresh store-seeded code so the table can
	// independently mutate the token request.
	seed := func(t *testing.T) (client *http.Client, srvURL, code string) {
		t.Helper()
		signer := newTestIDTokenSigner(t)
		srv, broker := newTestServerWithSigner(t, testConfig(), &fakeVerifier{
			claims: Claims{Subject: "x", Email: "x@y", EmailVerified: true},
		}, fake.NewSimpleClientset(), signer)
		id, err := broker.authCodeStore.Begin(&AuthCodeRequest{
			ClientID:            "client-abc",
			RedirectURI:         "http://127.0.0.1:55408/callback",
			CodeChallenge:       challenge,
			CodeChallengeMethod: "S256",
		})
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		c, _, err := broker.authCodeStore.Bind(id, &ExchangeResult{
			Claims: Claims{Subject: "x", Email: "x@y", EmailVerified: true},
		})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		return testClient(srv), srv.URL, c
	}

	type errCase struct {
		name     string
		form     func(code string) url.Values
		wantCode int
		wantErr  string
	}
	cases := []errCase{
		{
			name: "missing code",
			form: func(_ string) url.Values {
				return url.Values{
					"grant_type":    {"authorization_code"},
					"client_id":     {"client-abc"},
					"redirect_uri":  {"http://127.0.0.1:55408/callback"},
					"code_verifier": {codeVerifier},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_request",
		},
		{
			name: "missing code_verifier",
			form: func(code string) url.Values {
				return url.Values{
					"grant_type":   {"authorization_code"},
					"code":         {code},
					"client_id":    {"client-abc"},
					"redirect_uri": {"http://127.0.0.1:55408/callback"},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_request",
		},
		{
			name: "unknown code",
			form: func(_ string) url.Values {
				return url.Values{
					"grant_type":    {"authorization_code"},
					"code":          {"never-issued"},
					"client_id":     {"client-abc"},
					"redirect_uri":  {"http://127.0.0.1:55408/callback"},
					"code_verifier": {codeVerifier},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name: "wrong client_id",
			form: func(code string) url.Values {
				return url.Values{
					"grant_type":    {"authorization_code"},
					"code":          {code},
					"client_id":     {"wrong-client"},
					"redirect_uri":  {"http://127.0.0.1:55408/callback"},
					"code_verifier": {codeVerifier},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name: "wrong redirect_uri",
			form: func(code string) url.Values {
				return url.Values{
					"grant_type":    {"authorization_code"},
					"code":          {code},
					"client_id":     {"client-abc"},
					"redirect_uri":  {"http://127.0.0.1:9999/other"},
					"code_verifier": {codeVerifier},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name: "wrong code_verifier",
			form: func(code string) url.Values {
				return url.Values{
					"grant_type":    {"authorization_code"},
					"code":          {code},
					"client_id":     {"client-abc"},
					"redirect_uri":  {"http://127.0.0.1:55408/callback"},
					"code_verifier": {"wrong-verifier"},
				}
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, srvURL, code := seed(t)
			resp, err := client.Post(srvURL+"/device/token", "application/x-www-form-urlencoded",
				strings.NewReader(tc.form(code).Encode()))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.wantCode, body)
			}
			var doc deviceTokenError
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if doc.Error != tc.wantErr {
				t.Errorf("error = %q, want %q", doc.Error, tc.wantErr)
			}
		})
	}
}

// TestDeviceTokenAuthCodeReplayFails confirms one-shot semantics:
// a code that successfully redeems once returns invalid_grant on
// the next try. The store's deletion-on-success path is the gate;
// this test is the wire-level confirmation.
func TestDeviceTokenAuthCodeReplayFails(t *testing.T) {
	codeVerifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	signer := newTestIDTokenSigner(t)
	srv, broker := newTestServerWithSigner(t, testConfig(), &fakeVerifier{
		claims: Claims{Subject: "x", Email: "x@y", EmailVerified: true},
	}, fake.NewSimpleClientset(), signer)
	id, err := broker.authCodeStore.Begin(&AuthCodeRequest{
		ClientID:            "client-abc",
		RedirectURI:         "http://127.0.0.1:55408/callback",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	code, _, err := broker.authCodeStore.Bind(id, &ExchangeResult{
		Claims: Claims{Subject: "x", Email: "x@y", EmailVerified: true},
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {"client-abc"},
		"redirect_uri":  {"http://127.0.0.1:55408/callback"},
		"code_verifier": {codeVerifier},
	}
	client := testClient(srv)

	first, err := client.Post(srv.URL+"/device/token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	_ = first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.StatusCode)
	}

	second, err := client.Post(srv.URL+"/device/token", "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	defer func() { _ = second.Body.Close() }()
	if second.StatusCode != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", second.StatusCode)
	}
	var doc deviceTokenError
	if err := json.NewDecoder(second.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Error != "invalid_grant" {
		t.Errorf("replay error = %q, want invalid_grant", doc.Error)
	}
}
