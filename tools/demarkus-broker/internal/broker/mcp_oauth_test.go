package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestOAuthProtectedResourceMetadata(t *testing.T) {
	cfg := mcpTestConfig()
	ts := newTestMCPGateway(t, cfg, &fakeVerifier{})

	// Bare and RFC 9728 §3.1 path-inserted forms serve the same doc.
	for _, path := range []string{prmPath, prmPath + mcpPath} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if got := resp.Header.Get("Cache-Control"); got == "" {
				t.Error("Cache-Control header missing — intermediaries lose the caching hint that mirrors the OIDC well-known")
			}

			body, _ := io.ReadAll(resp.Body)
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("decode metadata: %v\nbody: %s", err, body)
			}
			wantResource := cfg.Server.MCP.PublicURL + mcpPath
			if got, _ := doc["resource"].(string); got != wantResource {
				t.Errorf("resource = %q, want %q (gateway host, not issuer host)", got, wantResource)
			}
			servers, _ := doc["authorization_servers"].([]any)
			if len(servers) != 1 {
				t.Fatalf("authorization_servers len = %d, want 1: %+v", len(servers), servers)
			}
			if got, _ := servers[0].(string); got != cfg.Server.PublicURL {
				t.Errorf("authorization_servers[0] = %q, want %q", got, cfg.Server.PublicURL)
			}
			methods, _ := doc["bearer_methods_supported"].([]any)
			if len(methods) != 1 || methods[0] != "header" {
				t.Errorf("bearer_methods_supported = %+v, want [\"header\"]", methods)
			}
			scopes, _ := doc["scopes_supported"].([]any)
			if len(scopes) == 0 {
				t.Error("scopes_supported empty — clients need a vocabulary hint")
			}
		})
	}
}

func TestOAuthAuthorizationServerNeverOnGateway(t *testing.T) {
	// RFC 8414 §3.3: AS metadata lives on the issuer's origin (the
	// management listener) only; the gateway must 404 unconditionally.
	ts := newTestMCPGateway(t, mcpTestConfig(), &fakeVerifier{})

	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("GET /.well-known/oauth-authorization-server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (AS metadata belongs on the issuer host only)", resp.StatusCode)
	}
}
