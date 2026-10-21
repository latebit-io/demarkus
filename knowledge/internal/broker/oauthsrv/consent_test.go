package oauthsrv

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"k8s.io/client-go/kubernetes/fake"
)

var consentNonceRe = regexp.MustCompile(`name="nonce" value="([^"]+)"`)

// consentFlow registers a client with a remote redirect and drives
// authorize and the IdP callback up to the consent page.
func consentFlow(t *testing.T) (client *http.Client, srv *httptest.Server, page string) {
	t.Helper()
	verifier := &brokertest.FakeVerifier{
		AuthURL: "https://idp.example.com/authorize",
		Claims:  core.Claims{Subject: "google|123", Email: "alice@example.com", EmailVerified: true},
	}
	cfg := brokertest.NewConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	srv, _ = newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

	resp := postJSON(t, srv, []byte(`{"client_name":"Claude Desktop","redirect_uris":["https://desktop.example/oauth/callback"]}`))
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&registered); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	_ = resp.Body.Close()

	client = redirectlessClient(t, srv)

	query := authorizeQuery()
	query.Set("client_id", registered.ClientID)
	query.Set("redirect_uri", "https://desktop.example/oauth/callback")
	authResp, err := client.Get(srv.URL + "/oauth/authorize?" + query.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d, want 302", authResp.StatusCode)
	}
	idp, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	cbReq, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/auth/callback?code=idp-code&state="+url.QueryEscape(idp.Query().Get("state")), http.NoBody)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(cbResp.Body)
	_ = cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want the consent page; body=%s", cbResp.StatusCode, body)
	}
	return client, srv, string(body)
}

// postConsent posts the decision form and returns the status, the body
// and the Location header, the body closed.
func postConsent(t *testing.T, client *http.Client, srv *httptest.Server, form url.Values) (status int, body, location string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+consentPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("consent: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(raw), resp.Header.Get("Location")
}

// A registered client with a remote redirect gets its code only after the
// signed-in user approves it on a page that names the client.
func TestOAuthAuthorizeRemoteClientNeedsConsent(t *testing.T) {
	client, srv, page := consentFlow(t)
	for _, want := range []string{"Claude Desktop", "desktop.example", "alice@example.com", `action="/oauth/consent"`} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	match := consentNonceRe.FindStringSubmatch(page)
	if match == nil {
		t.Fatal("consent page carries no nonce")
	}
	nonce := match[1]

	// A form with another nonce approves nothing.
	if status, _, _ := postConsent(t, client, srv, url.Values{"decision": {"approve"}, "nonce": {"forged"}}); status != http.StatusUnauthorized {
		t.Fatalf("forged nonce status = %d, want 401", status)
	}
	// The cookie is spent by that attempt: the user restarts the sign-in.
	if status, _, _ := postConsent(t, client, srv, url.Values{"decision": {"approve"}, "nonce": {nonce}}); status != http.StatusBadRequest {
		t.Fatalf("replay after a refused form status = %d, want 400", status)
	}

	client, srv, page = consentFlow(t)
	nonce = consentNonceRe.FindStringSubmatch(page)[1]
	status, body, location := postConsent(t, client, srv, url.Values{"decision": {"approve"}, "nonce": {nonce}})
	if status != http.StatusFound {
		t.Fatalf("approve status = %d, want 302; body=%s", status, body)
	}
	redirect, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	q := redirect.Query()
	if redirect.Host != "desktop.example" || q.Get("code") == "" || q.Get("state") != "client-state-nonce" || q.Get("iss") == "" {
		t.Fatalf("approved redirect = %q, want the client's callback with code, state and iss", redirect)
	}
	if status, _, _ := postConsent(t, client, srv, url.Values{"decision": {"approve"}, "nonce": {nonce}}); status != http.StatusBadRequest {
		t.Fatalf("second approval status = %d, want 400", status)
	}
}

func TestOAuthConsentDeniedRedirectsAccessDenied(t *testing.T) {
	client, srv, page := consentFlow(t)
	nonce := consentNonceRe.FindStringSubmatch(page)[1]
	status, _, location := postConsent(t, client, srv, url.Values{"decision": {"deny"}, "nonce": {nonce}})
	if status != http.StatusFound {
		t.Fatalf("deny status = %d, want 302", status)
	}
	redirect, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if q := redirect.Query(); q.Get("error") != "access_denied" || q.Get("code") != "" || q.Get("state") != "client-state-nonce" {
		t.Fatalf("deny redirect = %q, want access_denied with the state and no code", redirect)
	}
}

func TestOAuthConsentRefusesCrossOriginPost(t *testing.T) {
	client, srv, page := consentFlow(t)
	nonce := consentNonceRe.FindStringSubmatch(page)[1]
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+consentPath,
		strings.NewReader(url.Values{"decision": {"approve"}, "nonce": {nonce}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://attacker.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}
}
