package oauthsrv

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"k8s.io/client-go/kubernetes/fake"
)

// twoGatewayConfig has the knowledge gateway at the issuer and the memory
// gateway on its own host.
func twoGatewayConfig() *core.Config {
	cfg := brokertest.NewMemoryConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	return cfg
}

// postToken posts a token request and decodes the success or error body.
func postToken(t *testing.T, srv *httptest.Server, form url.Values) (int, deviceTokenSuccess, deviceTokenError) {
	t.Helper()
	resp, err := testClient(srv).Post(srv.URL+"/device/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /device/token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var ok deviceTokenSuccess
	var bad deviceTokenError
	_ = json.Unmarshal(body, &ok)
	_ = json.Unmarshal(body, &bad)
	return resp.StatusCode, ok, bad
}

func TestTokenResourceBindsTheGrant(t *testing.T) {
	cfg := twoGatewayConfig()
	signer := brokertest.NewTestIDTokenSigner(t)
	verifier := &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}
	srv, broker := newTestServerWithSigner(t, cfg, verifier, fake.NewSimpleClientset(), signer)
	knowledge, memory := cfg.Server.Resources()[0], cfg.Server.Resources()[1]

	codeVerifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(codeVerifier))
	id, err := broker.authCodeStore.Begin(&AuthCodeRequest{
		ClientID: "client-abc", RedirectURI: "http://127.0.0.1:55408/callback",
		CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]), CodeChallengeMethod: "S256",
		Resource: knowledge,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	exchange := core.ExchangeResult{Claims: brokertest.AliceClaims()}
	code, _, err := broker.authCodeStore.Bind(id, &exchange)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {"client-abc"},
		"redirect_uri": {"http://127.0.0.1:55408/callback"}, "code_verifier": {codeVerifier},
	}
	status, out, bad := postToken(t, srv, form)
	if status != http.StatusOK {
		t.Fatalf("auth code grant status = %d, error %q", status, bad.Error)
	}
	claims, err := signer.VerifyIDToken(out.IDToken, cfg.Server.PublicURL, cfg.Server.Resources(), time.Now())
	if err != nil {
		t.Fatalf("verify bound token: %v", err)
	}
	if claims.Resource != knowledge {
		t.Fatalf("resource = %q, want the knowledge resource", claims.Resource)
	}

	// The refresh grant keeps the binding without a resource parameter.
	status, refreshed, bad := postToken(t, srv, url.Values{"grant_type": {refreshGrantType}, "refresh_token": {out.RefreshToken}})
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d, error %q", status, bad.Error)
	}
	claims, err = signer.VerifyIDToken(refreshed.IDToken, cfg.Server.PublicURL, cfg.Server.Resources(), time.Now())
	if err != nil || claims.Resource != knowledge {
		t.Fatalf("refreshed resource = %q (err %v), want the knowledge resource", claims.Resource, err)
	}

	// The other gateway's resource is not the grant's: invalid_target.
	status, _, bad = postToken(t, srv, url.Values{"grant_type": {refreshGrantType}, "refresh_token": {out.RefreshToken}, "resource": {memory}})
	if status != http.StatusBadRequest || bad.Error != "invalid_target" {
		t.Fatalf("cross-gateway refresh = %d %q, want 400 invalid_target", status, bad.Error)
	}
	status, _, bad = postToken(t, srv, url.Values{"grant_type": {refreshGrantType}, "refresh_token": {out.RefreshToken}, "resource": {"https://elsewhere.example/mcp"}})
	if status != http.StatusBadRequest || bad.Error != "invalid_target" {
		t.Fatalf("unknown resource refresh = %d %q, want 400 invalid_target", status, bad.Error)
	}
}

func TestAuthorizeRefusesAnUnknownResource(t *testing.T) {
	cfg := twoGatewayConfig()
	srv, _ := newTestServer(t, cfg, &brokertest.FakeVerifier{}, fake.NewSimpleClientset())
	client := testClient(srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	q := authorizeQuery()
	q.Set("resource", "https://elsewhere.example/mcp")
	resp, err := client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	_, got := parseLocationQuery(t, resp)
	if resp.StatusCode != http.StatusFound || got.Get("error") != "invalid_target" {
		t.Fatalf("authorize = %d error=%q, want 302 invalid_target", resp.StatusCode, got.Get("error"))
	}

	// A known resource, in any spelling, is accepted.
	q.Set("resource", "HTTPS://Memory.Example.com/mcp/")
	resp, err = client.Get(srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize known: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "idp.example.com") {
		t.Fatalf("authorize known resource = %d %q, want the IdP redirect", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.PostForm(srv.URL+"/device/authorize", url.Values{"client_id": {"c"}, "resource": {"https://elsewhere.example/mcp"}})
	if err != nil {
		t.Fatalf("device authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var bad deviceTokenError
	_ = json.NewDecoder(resp.Body).Decode(&bad)
	if resp.StatusCode != http.StatusBadRequest || bad.Error != "invalid_target" {
		t.Fatalf("device authorize = %d %q, want 400 invalid_target", resp.StatusCode, bad.Error)
	}
}

func TestReadyzDropsOnDrain(t *testing.T) {
	cfg := brokertest.NewConfig()
	srv, broker := newTestServer(t, cfg, &brokertest.FakeVerifier{}, fake.NewSimpleClientset())
	resp, err := testClient(srv).Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz before drain = %d", resp.StatusCode)
	}
	broker.BeginDrain()
	resp, err = testClient(srv).Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz draining: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz while draining = %d, want 503", resp.StatusCode)
	}
}
