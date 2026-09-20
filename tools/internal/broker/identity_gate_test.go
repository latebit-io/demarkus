package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestGateIdentity(t *testing.T) {
	tests := []struct {
		name         string
		allowDomains []string
		claims       Claims
		wantErr      error
		wantEmail    string
	}{
		{"verified, no domain list", nil, Claims{Email: " Alice@Example.COM ", EmailVerified: true}, nil, "alice@example.com"},
		{"verified, allowed domain", []string{"example.com"}, Claims{Email: "a@example.com", EmailVerified: true, HD: "Example.com"}, nil, "a@example.com"},
		{"unverified email", nil, Claims{Email: "a@example.com"}, errIdentityUnverified, ""},
		{"foreign domain", []string{"example.com"}, Claims{Email: "a@evil.com", EmailVerified: true, HD: "evil.com"}, errIdentityDomain, ""},
		{"blank email", nil, Claims{Email: "  ", EmailVerified: true}, errIdentityNoEmail, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := tt.claims
			err := gateIdentity(tt.allowDomains, &claims)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("gateIdentity() = %v, want %v", err, tt.wantErr)
			}
			if err == nil && claims.Email != tt.wantEmail {
				t.Errorf("email = %q, want canonical %q", claims.Email, tt.wantEmail)
			}
		})
	}
}

// A refresh token minted before the org gate tightened must stop minting id tokens.
func TestDeviceTokenRefreshRegatesStoredClaims(t *testing.T) {
	tests := []struct {
		name   string
		claims Claims
	}{
		{"domain no longer allowed", Claims{Email: "a@evil.com", EmailVerified: true, Subject: "s1", HD: "evil.com"}},
		{"unverified email", Claims{Email: "a@example.com", Subject: "s2", HD: "example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := deviceTestConfig()
			cfg.OIDC.AllowDomains = []string{"example.com"}
			verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize"}
			srv, broker := newTestServerWithSigner(t, cfg, verifier, fake.NewSimpleClientset(), newTestIDTokenSigner(t))

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
		claims Claims
	}{
		{"foreign domain", Claims{Email: "a@evil.com", EmailVerified: true, Subject: "s1", HD: "evil.com"}},
		{"unverified email", Claims{Email: "a@example.com", Subject: "s2", HD: "example.com"}},
		{"blank email", Claims{Email: " ", EmailVerified: true, Subject: "s3", HD: "example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := deviceTestConfig()
			cfg.OIDC.AllowDomains = []string{"example.com"}
			verifier := &fakeVerifier{authURL: "https://idp.example.com/authorize", claims: tt.claims}
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
