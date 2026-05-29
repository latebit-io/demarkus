package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestOAuthProtectedResourceMetadata(t *testing.T) {
	cfg := mcpTestConfig()
	ts := newTestMCPGateway(t, cfg, &fakeVerifier{})

	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("GET /.well-known/oauth-protected-resource: %v", err)
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
	wantResource := cfg.Server.PublicURL + "/mcp"
	if got, _ := doc["resource"].(string); got != wantResource {
		t.Errorf("resource = %q, want %q", got, wantResource)
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
}

func TestOAuthAuthorizationServerMetadataReusesDiscoveryWhenWired(t *testing.T) {
	// The plan calls for /.well-known/oauth-authorization-server to
	// share the OIDC discovery body (broker is its own auth server,
	// RFC 8414 tolerates extras). This test pins the alias is mounted
	// when Discovery is wired into the Server.
	idpMux := http.NewServeMux()
	idpMux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"issuer":"https://idp.example.com","jwks_uri":"https://idp.example.com/jwks","authorization_endpoint":"https://idp.example.com/authz","token_endpoint":"https://idp.example.com/token"}`)
	})
	idp := httptest.NewServer(idpMux)
	t.Cleanup(idp.Close)

	d, err := NewDiscovery(context.Background(), DiscoveryConfig{
		BrokerURL: "https://broker.example.com",
		IdPIssuer: idp.URL,
	})
	if err != nil {
		t.Fatalf("NewDiscovery: %v", err)
	}

	cfg := mcpTestConfig()
	signer := newTestSigner(t)
	k8s := fake.NewSimpleClientset()
	// Discovery requires IDTokenSigner (NewServer enforces this).
	brokerSrv := NewServer(cfg, signer, &fakeVerifier{}, k8s, d, newTestIDTokenSigner(t), nil)
	handler := brokerSrv.MCPGateway("test")
	if handler == nil {
		t.Fatal("MCPGateway returned nil despite cfg.Server.MCP.Enabled=true")
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("GET /.well-known/oauth-authorization-server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode auth-server metadata: %v\nbody: %s", err, body)
	}
	// The Discovery handler rewrites issuer to the broker URL — that
	// rewrite is what makes the OIDC doc usable as RFC 8414 metadata
	// for a broker-as-auth-server deployment.
	if got, _ := doc["issuer"].(string); got != "https://broker.example.com" {
		t.Errorf("issuer = %q, want broker URL (Discovery override applied via shared body)", got)
	}
}

func TestOAuthAuthorizationServerAbsentWhenDiscoveryNotWired(t *testing.T) {
	// Discovery is optional in tests / dev wiring. When it's nil the
	// gateway must skip the auth-server metadata route — registering
	// a 404-or-empty handler would mislead RFC 8414 clients that
	// follow the resource_metadata → authorization_servers chain.
	ts := newTestMCPGateway(t, mcpTestConfig(), &fakeVerifier{})

	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (route should be absent when Discovery is nil)", resp.StatusCode)
	}
}
