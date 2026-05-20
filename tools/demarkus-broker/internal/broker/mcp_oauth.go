package broker

import (
	"encoding/json"
	"net/http"
)

// oauthProtectedResource implements RFC 9728 — Protected Resource
// Metadata. MCP clients (and any OAuth-aware caller) read this
// document to discover which authorization server issues tokens
// accepted by /mcp. The broker is its own authorization server, so
// `authorization_servers` is single-element and points at the broker's
// own PublicURL — the authorization-server metadata lives at
// /.well-known/oauth-authorization-server on the same origin.
//
// RFC 9728 §2 fields exposed:
//
//   - resource: canonical URL of the protected resource (the MCP
//     endpoint)
//   - authorization_servers: issuer URLs whose tokens this resource
//     accepts; one entry, the broker itself
//   - bearer_methods_supported: how clients present tokens — "header"
//     means Authorization: Bearer per RFC 6750
//   - scopes_supported: declared scope vocabulary. The broker emits
//     id_tokens (not narrowly-scoped access_tokens), but exposing the
//     coarse demarkus capabilities here documents the intent for
//     auth-server administrators and future scope-aware clients. The
//     per-world capability check still happens at the world server,
//     not here.
//
// Public, unauthenticated, cacheable: same posture as the OIDC
// discovery doc. Cache-Control mirrors the 5-minute TTL the rest of
// the well-known surface uses so an intermediary CDN treats them
// uniformly.
func (g *mcpGateway) oauthProtectedResource(w http.ResponseWriter, _ *http.Request) {
	base := g.srv.cfg.Server.PublicURL
	body := map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"mark.read", "mark.write"},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(body)
}
