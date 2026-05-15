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
	"time"
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
	v, err := NewVerifier(context.Background(), &cfg)
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
	_, err := NewVerifier(context.Background(), &OIDCConfig{
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
	authURL     string
	claims      Claims
	rawIDToken  string
	accessToken string
	expiry      time.Time
	exchErr     error
	verifyFn    func(raw string) (Claims, error) // optional per-token behavior for VerifyIDToken
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

func (f *fakeVerifier) Exchange(_ context.Context, _ string) (ExchangeResult, error) {
	if f.exchErr != nil {
		return ExchangeResult{}, f.exchErr
	}
	return ExchangeResult{
		Claims:      f.claims,
		RawIDToken:  f.rawIDToken,
		AccessToken: f.accessToken,
		Expiry:      f.expiry,
	}, nil
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, raw string) (Claims, error) {
	if f.verifyFn != nil {
		return f.verifyFn(raw)
	}
	return f.claims, nil
}

func TestCompositeVerifierPassThroughAuthAndExchange(t *testing.T) {
	primary := &fakeVerifier{
		authURL:    "https://idp.example.com/authorize",
		claims:     Claims{Email: "alice@x.com", EmailVerified: true, Subject: "g|alice"},
		rawIDToken: "primary-raw",
	}
	c := newCompositeVerifier(primary, newTestIDTokenSigner(t), "https://broker.example.com")

	if got := c.AuthCodeURL("nonce-1"); !strings.Contains(got, "nonce-1") {
		t.Errorf("AuthCodeURL = %q, want primary's URL with state", got)
	}
	res, err := c.Exchange(context.Background(), "code")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if res.RawIDToken != "primary-raw" {
		t.Errorf("Exchange forwarded to primary returned %q", res.RawIDToken)
	}
}

func TestCompositeVerifierVerifiesBrokerSigned(t *testing.T) {
	primary := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		return Claims{}, fmt.Errorf("primary should not be invoked for broker-signed tokens")
	}}
	signer := newTestIDTokenSigner(t)
	c := newCompositeVerifier(primary, signer, "https://broker.example.com")
	c.clock = func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

	raw, err := signer.Sign(Claims{
		Subject: "g|alice", Email: "alice@x.com", EmailVerified: true,
	}, "https://broker.example.com", time.Minute, c.clock())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	claims, err := c.VerifyIDToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Email != "alice@x.com" {
		t.Errorf("Email = %q", claims.Email)
	}
}

func TestCompositeVerifierFallsThroughOnUnknownKid(t *testing.T) {
	// Token signed by a DIFFERENT signer carries an unknown kid;
	// composite must fall through to the primary (IdP) verifier.
	otherSigner := newTestIDTokenSigner(t)
	raw, err := otherSigner.Sign(Claims{Email: "carol@x.com"}, "https://broker.example.com", time.Minute, time.Now())
	if err != nil {
		t.Fatalf("Sign on other signer: %v", err)
	}
	fallbackHit := false
	primary := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		fallbackHit = true
		return Claims{Email: "from-primary@x.com", EmailVerified: true}, nil
	}}
	brokerSigner := newTestIDTokenSigner(t)
	c := newCompositeVerifier(primary, brokerSigner, "https://broker.example.com")

	claims, err := c.VerifyIDToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if !fallbackHit {
		t.Errorf("primary not invoked for unknown-kid token")
	}
	if claims.Email != "from-primary@x.com" {
		t.Errorf("Email = %q, want from-primary path", claims.Email)
	}
}

func TestCompositeVerifierDoesNotFallThroughOnBrokerSignatureFail(t *testing.T) {
	// A token with the BROKER's kid but a tampered signature must
	// NOT fall through to the IdP — otherwise a forged token whose
	// kid points at the broker's would silently sneak through if
	// the IdP's verifier was permissive. This pins the dispatch
	// policy from the compositeVerifier doc comment.
	signer := newTestIDTokenSigner(t)
	now := time.Now()
	raw, err := signer.Sign(Claims{Subject: "u"}, "https://broker.example.com", time.Minute, now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Tamper a signature byte.
	parts := strings.SplitN(raw, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("unexpected token shape")
	}
	tamperedSig := []byte(parts[2])
	if tamperedSig[0] == 'A' {
		tamperedSig[0] = 'B'
	} else {
		tamperedSig[0] = 'A'
	}
	bad := parts[0] + "." + parts[1] + "." + string(tamperedSig)

	primary := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		t.Fatal("primary must not be invoked on broker-side signature failure")
		return Claims{}, nil
	}}
	c := newCompositeVerifier(primary, signer, "https://broker.example.com")
	if _, err := c.VerifyIDToken(context.Background(), bad); err == nil {
		t.Fatalf("tampered broker-signed token accepted")
	}
}

func TestCompositeVerifierNilSignerIsPassThrough(t *testing.T) {
	primary := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		return Claims{Email: "from-primary@x.com"}, nil
	}}
	c := newCompositeVerifier(primary, nil, "https://broker.example.com")
	claims, err := c.VerifyIDToken(context.Background(), "any-token")
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Email != "from-primary@x.com" {
		t.Errorf("Email = %q", claims.Email)
	}
}
