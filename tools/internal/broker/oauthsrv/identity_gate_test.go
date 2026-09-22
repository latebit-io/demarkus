package oauthsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/latebit-io/demarkus/tools/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"k8s.io/client-go/kubernetes/fake"
)

// A refresh token minted before the org gate tightened must stop minting id tokens.
func TestDeviceTokenRefreshRegatesStoredClaims(t *testing.T) {
	tests := []struct {
		name   string
		claims core.Claims
	}{
		{"domain no longer allowed", core.Claims{Email: "a@evil.com", EmailVerified: true, Subject: "s1", HD: "evil.com"}},
		{"unverified email", core.Claims{Email: "a@example.com", Subject: "s2", HD: "example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := deviceTestConfig()
			cfg.OIDC.AllowDomains = []string{"example.com"}
			verifier := &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize"}
			srv, broker := newTestServerWithSigner(t, cfg, verifier, fake.NewSimpleClientset(), brokertest.NewTestIDTokenSigner(t))

			rawRefresh, err := broker.refreshStore.Issue(context.Background(), &tt.claims, "", broker.cfg.Server.RefreshTokenTTL)
			if err != nil {
				t.Fatalf("refresh Issue: %v", err)
			}
			resp, err := testClient(srv).PostForm(srv.URL+"/device/token", url.Values{
				"grant_type":    {refreshGrantType},
				"refresh_token": {rawRefresh},
			})
			if err != nil {
				t.Fatalf("POST /device/token: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			var out deviceTokenError
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest || out.Error != "invalid_grant" {
				t.Fatalf("status %d error %q, want 400 invalid_grant", resp.StatusCode, out.Error)
			}
		})
	}
}

// The device callback must deny, not leave pending, an identity the gate refuses.
func TestDeviceCallbackGateRefusalDeniesGrant(t *testing.T) {
	tests := []struct {
		name   string
		claims core.Claims
	}{
		{"foreign domain", core.Claims{Email: "a@evil.com", EmailVerified: true, Subject: "s1", HD: "evil.com"}},
		{"unverified email", core.Claims{Email: "a@example.com", Subject: "s2", HD: "example.com"}},
		{"blank email", core.Claims{Email: " ", EmailVerified: true, Subject: "s3", HD: "example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := deviceTestConfig()
			cfg.OIDC.AllowDomains = []string{"example.com"}
			verifier := &brokertest.FakeVerifier{AuthURL: "https://idp.example.com/authorize", Claims: tt.claims}
			srv, _ := newTestServer(t, cfg, verifier, fake.NewSimpleClientset())

			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			client := testClient(srv)
			client.Jar = jar
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

			authResp, err := client.PostForm(srv.URL+"/device/authorize", url.Values{"client_id": {"demarkus-cli"}})
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			var auth deviceAuthorizeResponse
			if err := json.NewDecoder(authResp.Body).Decode(&auth); err != nil {
				t.Fatalf("decode authorize: %v", err)
			}
			_ = authResp.Body.Close()

			formResp, err := client.PostForm(srv.URL+"/device", url.Values{"user_code": {auth.UserCode}})
			if err != nil {
				t.Fatalf("form: %v", err)
			}
			_ = formResp.Body.Close()

			loginResp, err := client.Get(srv.URL + "/auth/login")
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			_ = loginResp.Body.Close()
			loc, err := url.Parse(loginResp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse login redirect: %v", err)
			}

			cbResp, err := client.Get(srv.URL + "/auth/callback?code=abc&state=" + url.QueryEscape(loc.Query().Get("state")))
			if err != nil {
				t.Fatalf("callback: %v", err)
			}
			_ = cbResp.Body.Close()

			if err := expectDeviceTokenError(t, srv, auth.DeviceCode, "access_denied"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
