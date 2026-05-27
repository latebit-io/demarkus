package broker

import (
	"net/http"
)

// oauthAuthorize is a stub authorization endpoint that always rejects
// with `unsupported_response_type`. It exists for one reason: MCP
// clients that follow the spec's authorization_code-first preference
// MUST be able to fetch SOME `authorization_endpoint` from the broker's
// discovery doc and get a clear "not supported here" signal — otherwise
// the field defaults to the upstream IdP (e.g. accounts.google.com)
// which the broker's DCR-minted client_id was never registered with,
// surfacing as a confusing `invalid_client` from the IdP itself.
//
// Returning a JSON 400 with a standard OAuth error code lets a
// well-behaved MCP client either surface the error to the user or
// (better) consult `grant_types_supported` in the same discovery doc
// and pivot to the device_code grant.
//
// Probe semantics: this handler ships paired with the
// `authorization_endpoint` + `grant_types_supported` discovery overrides
// in applyDiscoveryOverrides. If Claude Code's MCP SDK pivots to
// device flow on its own, this stub stays as-is. If the SDK insists on
// authorization_code, the broker needs a real OAuth authorization-code
// grant (handler that mediates between the MCP client and the upstream
// IdP via /auth/callback); at that point this file becomes the real
// authorize handler instead of a polite rejection.
//
// Authentication: none. The endpoint is anonymous-public — same posture
// as the upstream IdP's authorization endpoint would be. IP rate
// limiter (server.go) caps per-IP churn.
//
// HTTP method: GET and POST both accepted per RFC 6749 §3.1 ("The
// authorization server MUST support the use of the HTTP GET method
// [...] and MAY support the use of the HTTP POST method as well").
// Routing them at the same handler keeps the rejection uniform —
// neither shape is valid here.
func (s *Server) oauthAuthorize(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":             "unsupported_response_type",
		"error_description": "this broker does not support the authorization_code grant; clients should consult grant_types_supported in the discovery document and use the device_code grant via device_authorization_endpoint",
	})
}
