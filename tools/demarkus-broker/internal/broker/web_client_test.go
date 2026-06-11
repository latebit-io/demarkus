package broker

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// testWebClientSecret is the plaintext secret the registered test web
// client authenticates with; only its sha256 hash enters the config.
const testWebClientSecret = "web-client-secret-0123456789abcdef"

const testWebClientID = "library-web"

const testWebRedirectURI = "https://library.example.com/auth/callback"

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// webTestConfig is testConfig plus one registered confidential web
// client — the Universe Library shape from the phase-1b plan.
func webTestConfig() *Config {
	cfg := testConfig()
	cfg.WebClients = []WebClientConfig{{
		ClientID:         testWebClientID,
		ClientSecretHash: sha256Hex(testWebClientSecret),
		RedirectURIs:     []string{testWebRedirectURI},
		Name:             "Universe Library",
	}}
	return cfg
}

func TestValidateWebClients(t *testing.T) {
	valid := func() []WebClientConfig {
		return []WebClientConfig{{
			ClientID:         testWebClientID,
			ClientSecretHash: sha256Hex(testWebClientSecret),
			RedirectURIs:     []string{testWebRedirectURI},
		}}
	}
	tests := []struct {
		name    string
		mutate  func([]WebClientConfig) []WebClientConfig
		wantErr string
	}{
		{
			"valid entry passes",
			func(c []WebClientConfig) []WebClientConfig { return c },
			"",
		},
		{
			"empty registry passes",
			func([]WebClientConfig) []WebClientConfig { return nil },
			"",
		},
		{
			"missing clientID",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientID = ""; return c },
			"clientID is required",
		},
		{
			"duplicate clientID",
			func(c []WebClientConfig) []WebClientConfig { return append(c, c[0]) },
			"duplicate clientID",
		},
		{
			"missing secret hash",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientSecretHash = ""; return c },
			"clientSecretHash must be 64 hex chars",
		},
		{
			"plaintext secret instead of hash",
			func(c []WebClientConfig) []WebClientConfig { c[0].ClientSecretHash = testWebClientSecret; return c },
			"clientSecretHash must be 64 hex chars",
		},
		{
			"uppercase hash normalized",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].ClientSecretHash = strings.ToUpper(c[0].ClientSecretHash)
				return c
			},
			"",
		},
		{
			"no redirect URIs",
			func(c []WebClientConfig) []WebClientConfig { c[0].RedirectURIs = nil; return c },
			"at least one redirectURI is required",
		},
		{
			"http redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"http://library.example.com/cb"}
				return c
			},
			"scheme must be https",
		},
		{
			"https loopback rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://localhost:8443/cb"}
				return c
			},
			"loopback hosts",
		},
		{
			"userinfo redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://user@library.example.com/cb"}
				return c
			},
			"userinfo is not allowed",
		},
		{
			"fragment redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"https://library.example.com/cb#frag"}
				return c
			},
			"fragment is not allowed",
		},
		{
			"relative redirect rejected",
			func(c []WebClientConfig) []WebClientConfig {
				c[0].RedirectURIs = []string{"/auth/callback"}
				return c
			},
			"scheme must be https",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWebClients(tt.mutate(valid()))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateWebClientsNormalizesHash(t *testing.T) {
	clients := []WebClientConfig{{
		ClientID:         testWebClientID,
		ClientSecretHash: " " + strings.ToUpper(sha256Hex(testWebClientSecret)) + " ",
		RedirectURIs:     []string{testWebRedirectURI},
	}}
	if err := validateWebClients(clients); err != nil {
		t.Fatalf("validateWebClients: %v", err)
	}
	if got, want := clients[0].ClientSecretHash, sha256Hex(testWebClientSecret); got != want {
		t.Errorf("hash not normalized: got %q, want %q", got, want)
	}
}

func TestParseClientAuth(t *testing.T) {
	// makeReq builds a /device/token-shaped POST. basicID/basicSecret
	// empty means no Authorization header.
	makeReq := func(form url.Values, basicID, basicSecret string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/device/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if basicID != "" || basicSecret != "" {
			r.SetBasicAuth(basicID, basicSecret)
		}
		return r
	}
	tests := []struct {
		name        string
		form        url.Values
		basicID     string
		basicSecret string
		want        clientAuth
		wantErr     bool
	}{
		{
			name: "no credentials is the public shape",
			form: url.Values{"client_id": {"client-abc"}},
			want: clientAuth{ClientID: "client-abc"},
		},
		{
			name: "client_secret_post",
			form: url.Values{"client_id": {testWebClientID}, "client_secret": {testWebClientSecret}},
			want: clientAuth{ClientID: testWebClientID, Secret: testWebClientSecret},
		},
		{
			name:        "basic auth",
			form:        url.Values{},
			basicID:     testWebClientID,
			basicSecret: testWebClientSecret,
			want:        clientAuth{ClientID: testWebClientID, Secret: testWebClientSecret},
		},
		{
			name:        "basic auth halves are form-urlencoded (RFC 6749 §2.3.1)",
			form:        url.Values{},
			basicID:     "web%20app",
			basicSecret: "s%3Acret",
			want:        clientAuth{ClientID: "web app", Secret: "s:cret"},
		},
		{
			name:        "basic plus matching form client_id is fine",
			form:        url.Values{"client_id": {testWebClientID}},
			basicID:     testWebClientID,
			basicSecret: testWebClientSecret,
			want:        clientAuth{ClientID: testWebClientID, Secret: testWebClientSecret},
		},
		{
			name:        "basic plus contradicting form client_id rejected",
			form:        url.Values{"client_id": {"someone-else"}},
			basicID:     testWebClientID,
			basicSecret: testWebClientSecret,
			wantErr:     true,
		},
		{
			name:        "basic plus client_secret_post rejected (two methods)",
			form:        url.Values{"client_secret": {testWebClientSecret}},
			basicID:     testWebClientID,
			basicSecret: testWebClientSecret,
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseClientAuth(makeReq(tt.form, tt.basicID, tt.basicSecret))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want error; got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClientAuth: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestVerifyWebClientSecret(t *testing.T) {
	wc := &WebClientConfig{
		ClientID:         testWebClientID,
		ClientSecretHash: sha256Hex(testWebClientSecret),
	}
	tests := []struct {
		name   string
		secret string
		want   bool
	}{
		{"correct secret", testWebClientSecret, true},
		{"wrong secret", "not-the-secret", false},
		{"empty secret", "", false},
		{"hash itself presented as secret", sha256Hex(testWebClientSecret), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyWebClientSecret(wc, tt.secret); got != tt.want {
				t.Errorf("verifyWebClientSecret = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestOAuthAuthorizeWebClient locks the registry branch on
// /oauth/authorize: a registered client gets its https redirect
// allowlist (exact match) and nothing else — the loopback exemption
// belongs to unregistered native clients only.
func TestOAuthAuthorizeWebClient(t *testing.T) {
	verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
	srv, _ := newTestServer(t, webTestConfig(), verifier, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	webQuery := func() url.Values {
		q := authorizeQuery()
		q.Set("client_id", testWebClientID)
		q.Set("redirect_uri", testWebRedirectURI)
		return q
	}

	tests := []struct {
		name       string
		mutate     func(url.Values)
		wantStatus int
	}{
		{
			name:       "registered redirect accepted",
			mutate:     func(url.Values) {},
			wantStatus: http.StatusFound,
		},
		{
			name:       "unregistered https redirect rejected",
			mutate:     func(q url.Values) { q.Set("redirect_uri", "https://attacker.example/steal") },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "prefix variant of registered redirect rejected (exact match only)",
			mutate:     func(q url.Values) { q.Set("redirect_uri", testWebRedirectURI+"/extra") },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "loopback redirect rejected for registered client",
			mutate:     func(q url.Values) { q.Set("redirect_uri", "http://127.0.0.1:55408/callback") },
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := webQuery()
			tt.mutate(q)
			resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tt.wantStatus, body)
			}
			if tt.wantStatus == http.StatusFound {
				if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, verifier.authURL) {
					t.Errorf("Location = %q, want IdP redirect", loc)
				}
				return
			}
			doc := decodeJSONError(t, resp.Body)
			if doc.Error != "invalid_request" {
				t.Errorf("error = %q, want invalid_request", doc.Error)
			}
			if got := resp.Header.Get("Location"); got != "" {
				t.Errorf("Location set on rejected redirect: %q", got)
			}
		})
	}
}

// seedWebClientCode plants a bound auth-code-store entry for the
// registered web client and returns the redeemable code.
func seedWebClientCode(t *testing.T, brokerSrv *Server, challenge string) string {
	t.Helper()
	id, err := brokerSrv.authCodeStore.Begin(&AuthCodeRequest{
		ClientID:            testWebClientID,
		RedirectURI:         testWebRedirectURI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	code, _, err := brokerSrv.authCodeStore.Bind(id, &ExchangeResult{
		Claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true},
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return code
}

// TestDeviceTokenAuthCodeWebClient locks client authentication on the
// authorization_code grant for registered confidential clients: the
// secret is required, verified constant-time against the registry
// hash, and a failed attempt does not burn the code.
func TestDeviceTokenAuthCodeWebClient(t *testing.T) {
	codeVerifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	tokenForm := func(code string) url.Values {
		return url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {testWebRedirectURI},
			"code_verifier": {codeVerifier},
		}
	}
	postToken := func(t *testing.T, client *http.Client, srvURL string, form url.Values, basic bool) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srvURL+"/device/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if basic {
			req.SetBasicAuth(testWebClientID, testWebClientSecret)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /device/token: %v", err)
		}
		return resp
	}

	t.Run("basic auth succeeds and binds the refresh token", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		code := seedWebClientCode(t, brokerSrv, challenge)
		resp := postToken(t, testClient(srv), srv.URL, tokenForm(code), true)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
		var doc deviceTokenSuccess
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.RefreshToken == "" {
			t.Fatal("missing refresh_token")
		}
		// The minted refresh token must be client-bound: refreshing it
		// without client auth is rejected.
		noAuth, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
			"grant_type":    {refreshGrantType},
			"refresh_token": {doc.RefreshToken},
		})
		if err != nil {
			t.Fatalf("refresh POST: %v", err)
		}
		defer func() { _ = noAuth.Body.Close() }()
		if noAuth.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(noAuth.Body)
			t.Fatalf("unauthenticated refresh of bound token: status = %d, want 401; body=%s", noAuth.StatusCode, body)
		}
	})

	t.Run("client_secret_post succeeds", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		code := seedWebClientCode(t, brokerSrv, challenge)
		form := tokenForm(code)
		form.Set("client_id", testWebClientID)
		form.Set("client_secret", testWebClientSecret)
		resp := postToken(t, testClient(srv), srv.URL, form, false)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
	})

	t.Run("missing secret is invalid_client and preserves the code", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		code := seedWebClientCode(t, brokerSrv, challenge)
		form := tokenForm(code)
		form.Set("client_id", testWebClientID)
		resp := postToken(t, testClient(srv), srv.URL, form, false)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Errorf("WWW-Authenticate = %q, want Basic challenge", got)
		}
		var doc deviceTokenError
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.Error != "invalid_client" {
			t.Errorf("error = %q, want invalid_client", doc.Error)
		}
		// The failed attempt must not consume the code — a retry with
		// the correct secret succeeds.
		retry := postToken(t, testClient(srv), srv.URL, tokenForm(code), true)
		defer func() { _ = retry.Body.Close() }()
		if retry.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(retry.Body)
			t.Fatalf("retry status = %d, want 200 (code burned by failed client auth); body=%s", retry.StatusCode, body)
		}
	})

	t.Run("wrong secret is invalid_client", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		code := seedWebClientCode(t, brokerSrv, challenge)
		form := tokenForm(code)
		form.Set("client_id", testWebClientID)
		form.Set("client_secret", "wrong-secret")
		resp := postToken(t, testClient(srv), srv.URL, form, false)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, body)
		}
	})

	t.Run("both basic and post secret is invalid_request", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		code := seedWebClientCode(t, brokerSrv, challenge)
		form := tokenForm(code)
		form.Set("client_secret", testWebClientSecret)
		resp := postToken(t, testClient(srv), srv.URL, form, true)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
		}
		var doc deviceTokenError
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.Error != "invalid_request" {
			t.Errorf("error = %q, want invalid_request", doc.Error)
		}
	})
}

// TestDeviceTokenRefreshWebClientBinding locks the refresh-grant gate
// for client-bound tokens: the bound client must authenticate; an
// unbound token (device flow / loopback) is unaffected by the registry.
func TestDeviceTokenRefreshWebClientBinding(t *testing.T) {
	issueBound := func(t *testing.T, brokerSrv *Server) string {
		t.Helper()
		raw, err := brokerSrv.refreshStore.Issue(t.Context(),
			&Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: true},
			testWebClientID, time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		return raw
	}
	refresh := func(t *testing.T, client *http.Client, srvURL, rawRefresh string, mutate func(*http.Request)) *http.Response {
		t.Helper()
		form := url.Values{
			"grant_type":    {refreshGrantType},
			"refresh_token": {rawRefresh},
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srvURL+"/device/token", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if mutate != nil {
			mutate(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		return resp
	}

	t.Run("bound token with correct basic auth refreshes", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw := issueBound(t, brokerSrv)
		resp := refresh(t, testClient(srv), srv.URL, raw, func(r *http.Request) {
			r.SetBasicAuth(testWebClientID, testWebClientSecret)
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
	})

	t.Run("bound token without client auth is invalid_client", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw := issueBound(t, brokerSrv)
		resp := refresh(t, testClient(srv), srv.URL, raw, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 401; body=%s", resp.StatusCode, body)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Errorf("WWW-Authenticate = %q, want Basic challenge", got)
		}
		var doc deviceTokenError
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if doc.Error != "invalid_client" {
			t.Errorf("error = %q, want invalid_client", doc.Error)
		}
	})

	t.Run("bound token with wrong secret is invalid_client", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw := issueBound(t, brokerSrv)
		resp := refresh(t, testClient(srv), srv.URL, raw, func(r *http.Request) {
			r.SetBasicAuth(testWebClientID, "wrong-secret")
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("bound token under a different client_id is invalid_client", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw := issueBound(t, brokerSrv)
		resp := refresh(t, testClient(srv), srv.URL, raw, func(r *http.Request) {
			r.SetBasicAuth("someone-else", testWebClientSecret)
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("bound token whose client was deregistered is invalid_client", func(t *testing.T) {
		// Config WITHOUT the registry entry, but the store carries a
		// bound record (issued before the deregistration).
		srv, brokerSrv := newTestServerWithSigner(t, testConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw := issueBound(t, brokerSrv)
		resp := refresh(t, testClient(srv), srv.URL, raw, func(r *http.Request) {
			r.SetBasicAuth(testWebClientID, testWebClientSecret)
		})
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("unbound token still refreshes without client auth", func(t *testing.T) {
		srv, brokerSrv := newTestServerWithSigner(t, webTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset(), newTestIDTokenSigner(t))
		raw, err := brokerSrv.refreshStore.Issue(t.Context(),
			&Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		resp := refresh(t, testClient(srv), srv.URL, raw, nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
	})
}

func TestDiscoveryOverridesTokenEndpointAuthMethods(t *testing.T) {
	idp := newFakeDiscoveryIdP(t)
	d, _ := newTestDiscovery(t, idp, time.Minute)
	doc := serveAndDecode(t, d)
	raw, ok := doc["token_endpoint_auth_methods_supported"].([]any)
	if !ok {
		t.Fatalf("token_endpoint_auth_methods_supported missing or wrong type: %v", doc["token_endpoint_auth_methods_supported"])
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{"none", "client_secret_basic", "client_secret_post"}
	if len(got) != len(want) {
		t.Fatalf("methods = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("methods = %v, want %v", got, want)
		}
	}
}
