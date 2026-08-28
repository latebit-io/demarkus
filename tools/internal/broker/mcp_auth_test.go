package broker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// resourceMetadataRE pulls the resource_metadata="..." parameter out
// of a WWW-Authenticate Bearer challenge so the assertion is
// whitespace-tolerant (RFC 6750 lets the params be in any order with
// arbitrary linear whitespace between them).
var resourceMetadataRE = regexp.MustCompile(`resource_metadata="([^"]+)"`)

func TestGatewayAuthMissingBearerReturns401WithChallenge(t *testing.T) {
	cfg := mcpTestConfig()
	ts := newTestMCPGateway(t, cfg, &fakeVerifier{})

	// Bare POST with no Authorization header — gateway auth must
	// reject with 401 and the RFC 6750/9728 challenge that points
	// the client at the protected-resource metadata document.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if challenge == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Errorf("WWW-Authenticate = %q, want Bearer scheme", challenge)
	}
	if !strings.Contains(challenge, `realm="demarkus-knowledge-broker"`) {
		t.Errorf("WWW-Authenticate missing realm: %q", challenge)
	}
	if !strings.Contains(challenge, `error="invalid_request"`) {
		t.Errorf("WWW-Authenticate missing error=\"invalid_request\" (RFC 6750 §3.1): %q", challenge)
	}
	m := resourceMetadataRE.FindStringSubmatch(challenge)
	if m == nil {
		t.Fatalf("WWW-Authenticate missing resource_metadata param: %q", challenge)
	}
	wantMetadata := cfg.Server.MCP.PublicURL + "/.well-known/oauth-protected-resource"
	if m[1] != wantMetadata {
		t.Errorf("resource_metadata = %q, want %q", m[1], wantMetadata)
	}
}

func TestGatewayAuthInvalidBearerReturns401WithErrorCode(t *testing.T) {
	// Verifier that always rejects — exercises the err-path of
	// gatewayAuth. The challenge must report error="invalid_token"
	// so RFC 6750-aware clients distinguish "bearer required" from
	// "bearer present but bad" without parsing the body.
	v := &fakeVerifier{verifyFn: func(string) (Claims, error) {
		return Claims{}, errors.New("forged signature")
	}}
	ts := newTestMCPGateway(t, mcpTestConfig(), v)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want error=\"invalid_token\"", challenge)
	}
}

func TestGatewayAuthRejectsUnverifiedEmail(t *testing.T) {
	// Plan v5 Non-Negotiable "Single identity dimension": the
	// MCP gateway refuses unverified-email id_tokens at the
	// boundary so the broker has one consistent identity gate
	// across /me/install, /auth/callback, and /mcp.
	// Returning invalid_token (not invalid_request) matches
	// RFC 6750 §3.1 — the bearer is present but its claims
	// don't meet our requirements.
	v := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "alice@example.com", EmailVerified: false}}
	ts := newTestMCPGateway(t, mcpTestConfig(), v)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice-token")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want error=\"invalid_token\"", challenge)
	}
}

func TestGatewayAuthRejectsEmptyEmailClaim(t *testing.T) {
	// EmailVerified=true with Email="" can occur if an IdP is
	// misconfigured to emit verified=true without surfacing the
	// address. Letting it through would collapse every such
	// caller onto a shared "" session key in the cache. The
	// gateway rejects at the boundary; the session cache also
	// guards as defense-in-depth (errInvalidEmail) but tests
	// here pin the boundary behavior.
	v := &fakeVerifier{claims: Claims{Subject: "google|alice", Email: "   ", EmailVerified: true}}
	ts := newTestMCPGateway(t, mcpTestConfig(), v)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice-token")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(challenge, `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want error=\"invalid_token\"", challenge)
	}
}

func TestGatewayAuthValidBearerStashesClaimsOnContext(t *testing.T) {
	// Compose gatewayAuth around a recording handler so we can pin
	// that valid bearers land on the next handler WITH the claims
	// already attached. Mirrors TestRequireAuthSucceeds in
	// ratelimit_test.go but for the MCP-side middleware.
	cfg := mcpTestConfig()
	wantSubject := "google|alice"
	v := &fakeVerifier{claims: Claims{Subject: wantSubject, Email: "alice@example.com", EmailVerified: true}}

	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	brokerSrv := NewServer(cfg, signer, v, NewK8sSecretStore(k8s), nil, nil, nil)

	var seen *Claims
	var sawClaims bool
	recorder := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, sawClaims = claimsFromCtx(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := brokerSrv.gatewayAuth(recorder)

	req := httptest.NewRequest(http.MethodPost, "/mcp", http.NoBody).WithContext(context.Background())
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !sawClaims {
		t.Fatal("downstream handler did not see claims on context")
	}
	if seen.Subject != wantSubject {
		t.Errorf("claims.Subject = %q, want %q", seen.Subject, wantSubject)
	}
}

func TestBearerAuthEnforcesAllowDomains(t *testing.T) {
	tests := []struct {
		name         string
		wrap         func(*Server, http.Handler) http.Handler
		mcpChallenge bool
		brokerSigned bool
		hd           string
		wantStatus   int
	}{
		{"requireAuth accepts direct IdP bearer", (*Server).requireAuth, false, false, "latebit.io", http.StatusNoContent},
		{"requireAuth rejects direct IdP bearer", (*Server).requireAuth, false, false, "outside.example", http.StatusUnauthorized},
		{"requireAuth accepts refreshed bearer", (*Server).requireAuth, false, true, "latebit.io", http.StatusNoContent},
		{"requireAuth rejects refreshed bearer", (*Server).requireAuth, false, true, "outside.example", http.StatusUnauthorized},
		{"gatewayAuth accepts direct IdP bearer", (*Server).gatewayAuth, true, false, "latebit.io", http.StatusNoContent},
		{"gatewayAuth rejects direct IdP bearer", (*Server).gatewayAuth, true, false, "outside.example", http.StatusUnauthorized},
		{"gatewayAuth accepts refreshed bearer", (*Server).gatewayAuth, true, true, "latebit.io", http.StatusNoContent},
		{"gatewayAuth rejects refreshed bearer", (*Server).gatewayAuth, true, true, "outside.example", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mcpTestConfig()
			cfg.OIDC.AllowDomains = []string{"latebit.io"}
			idTokenSigner := newTestIDTokenSigner(t)
			primary := &fakeVerifier{verifyFn: func(raw string) (Claims, error) {
				if raw != "idp-token" {
					return Claims{}, errors.New("unexpected IdP bearer")
				}
				return Claims{
					Subject:       "google|alice",
					Email:         "alice@latebit.io",
					EmailVerified: true,
					HD:            tt.hd,
				}, nil
			}}
			srv := NewServer(cfg, newTestSigner(t), primary, NewK8sSecretStore(fake.NewSimpleClientset()), nil, idTokenSigner, nil)

			raw := "idp-token"
			if tt.brokerSigned {
				var err error
				raw, err = idTokenSigner.Sign(&Claims{
					Subject:       "google|alice",
					Email:         "alice@latebit.io",
					EmailVerified: true,
					HD:            tt.hd,
				}, cfg.Server.PublicURL, time.Hour, time.Now())
				if err != nil {
					t.Fatalf("Sign: %v", err)
				}
			}

			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
			req.Header.Set("Authorization", "Bearer "+raw)
			rec := httptest.NewRecorder()
			tt.wrap(srv, next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			if tt.wantStatus == http.StatusUnauthorized && tt.mcpChallenge {
				if !strings.Contains(challenge, `error="invalid_token"`) {
					t.Errorf("WWW-Authenticate = %q, want invalid_token challenge", challenge)
				}
			} else if challenge != "" {
				t.Errorf("WWW-Authenticate = %q, want empty", challenge)
			}
		})
	}
}
