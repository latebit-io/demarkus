package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeIdP serves the minimum OIDC discovery surface NewVerifier touches:
// /.well-known/openid-configuration and an empty JWKS. It does not
// implement the token endpoint — Exchange tests use the fake Verifier
// shipped for issuer/server-layer tests.
func newFakeIdP(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/authorize",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"keys": []}`)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestNewVerifierDiscoversAndBuildsAuthURL(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := OIDCConfig{
		Issuer:       idp.URL,
		ClientID:     "test-client",
		ClientSecret: "secret",
		RedirectURL:  "https://broker.example.com/auth/callback",
	}
	v, err := NewVerifier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	authURL := v.AuthCodeURL("the-nonce")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authURL: %v", err)
	}
	q := u.Query()
	checks := map[string]string{
		"client_id":     "test-client",
		"redirect_uri":  "https://broker.example.com/auth/callback",
		"state":         "the-nonce",
		"response_type": "code",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("auth URL param %s = %q, want %q", k, got, want)
		}
	}
	if scope := q.Get("scope"); !strings.Contains(scope, "openid") || !strings.Contains(scope, "email") {
		t.Errorf("scope = %q, want openid + email", scope)
	}
	if !strings.HasPrefix(authURL, idp.URL+"/authorize") {
		t.Errorf("authURL = %q, want prefix %q", authURL, idp.URL+"/authorize")
	}
}

func TestNewVerifierFailsOnBadIssuer(t *testing.T) {
	_, err := NewVerifier(context.Background(), OIDCConfig{
		Issuer:       "http://127.0.0.1:1", // unreachable
		ClientID:     "x",
		ClientSecret: "y",
		RedirectURL:  "z",
	})
	if err == nil {
		t.Fatal("expected discovery error on unreachable issuer, got nil")
	}
}

// fakeVerifier is the test double issuer/server tests use. It implements
// the Verifier interface with predetermined claims and is not coupled to
// the OIDC library.
type fakeVerifier struct {
	authURL  string
	claims   Claims
	exchErr  error
	verifyFn func(raw string) (Claims, error) // optional per-token behavior for VerifyIDToken
}

func (f *fakeVerifier) AuthCodeURL(state string) string {
	if f.authURL == "" {
		return "https://idp.example.com/authorize?state=" + url.QueryEscape(state)
	}
	if strings.Contains(f.authURL, "?") {
		return f.authURL + "&state=" + url.QueryEscape(state)
	}
	return f.authURL + "?state=" + url.QueryEscape(state)
}

func (f *fakeVerifier) Exchange(_ context.Context, _ string) (Claims, error) {
	if f.exchErr != nil {
		return Claims{}, f.exchErr
	}
	return f.claims, nil
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, raw string) (Claims, error) {
	if f.verifyFn != nil {
		return f.verifyFn(raw)
	}
	return f.claims, nil
}
