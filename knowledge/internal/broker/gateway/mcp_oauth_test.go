package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

func TestOAuthProtectedResourceMetadata(t *testing.T) {
	cfg := mcpTestConfig()
	ts := newTestMCPGateway(t, cfg, &brokertest.FakeVerifier{})

	// Bare and RFC 9728 §3.1 path-inserted forms serve the same doc.
	for _, path := range []string{prmPath, prmPath + core.MCPPath} {
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

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatalf("decode metadata: %v\nbody: %s", err, body)
			}
			wantResource := cfg.Server.MCP.PublicURL + core.MCPPath
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
	// RFC 8414 §3.3: AS metadata lives on the issuer's origin only; the
	// gateway must 404 (the state that used to mount the alias).
	cfg := mcpTestConfig()
	ts := newTestMCPGatewayWith(t, cfg, &brokertest.FakeVerifier{}, &fakeDispatcher{})

	resp, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("GET /.well-known/oauth-authorization-server: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (AS metadata belongs on the issuer host only)", resp.StatusCode)
	}
}

// TestRegisterSelectsTheGatewayByHost: on one mux the memory gateway answers
// its hostname and the knowledge gateway every other host.
func TestRegisterSelectsTheGatewayByHost(t *testing.T) {
	cfg := brokertest.NewMemoryConfig()
	cfg.Server.PublicURL = "https://broker.example.com"
	cfg.Server.MCP.PublicURL = "https://gateway.example.com"
	verifier := &brokertest.FakeVerifier{Claims: brokertest.AliceClaims()}
	mux := http.NewServeMux()
	newGatewayFixture(t, cfg, verifier, KnowledgeProfile()).gateway(&fakeDispatcher{}).Register(mux, "")
	newGatewayFixture(t, cfg, verifier, MemoryProfile()).gateway(&fakeDispatcher{}).Register(mux, cfg.Server.Memory.Host())

	resourceFor := func(host string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource"+core.MCPPath, http.NoBody)
		req.Host = host
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d", host, rec.Code)
		}
		var doc struct {
			Resource string `json:"resource"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("host %q: decode: %v", host, err)
		}
		return doc.Resource
	}
	if got := resourceFor("memory.example.com:443"); got != "https://memory.example.com/mcp" {
		t.Errorf("memory host resource = %q", got)
	}
	for _, host := range []string{"gateway.example.com", "broker.example.com", "10.0.0.7:8080"} {
		if got := resourceFor(host); got != "https://gateway.example.com/mcp" {
			t.Errorf("host %q resource = %q, want the knowledge gateway", host, got)
		}
	}
	// The memory host's /mcp is the memory gateway's challenge, not the knowledge one's.
	req := httptest.NewRequest(http.MethodPost, core.MCPPath, http.NoBody)
	req.Host = "memory.example.com"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "https://memory.example.com/.well-known") {
		t.Errorf("memory host /mcp = %d %q, want the memory gateway's challenge", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
