package broker

import (
	"fmt"
	"net/http"
	"strings"
)

// gatewayAuth is the MCP-gateway-specific authentication middleware.
// Same id_token verification chain as requireAuth (composite verifier:
// broker-signed leg first, IdP-signed fallback — see oidc.go), but the
// 401 response carries an RFC 6750 + RFC 9728 WWW-Authenticate
// challenge so a compliant MCP client discovers the OAuth flow from
// the response itself, without out-of-band configuration. The
// `resource_metadata` parameter points at the broker's protected-
// resource metadata document (RFC 9728), which in turn declares the
// authorization-server URL — completing the discovery chain per the
// MCP spec's OAuth integration.
//
// Slice 2 adds the unverified-email gate: the broker's identity
// primitive is canonical verified email (see plan v5 Non-Negotiable
// "Single identity dimension"). Rejecting unverified-email tokens at
// the gateway boundary keeps the MCP surface aligned with
// ErrEmailUnverified — every other broker surface (/auth/callback,
// /me/install) already refuses unverified identities, and the gateway
// must not be the easy door in. An empty email claim is also rejected
// because the canonical email is the identity primitive downstream and
// an empty key would collapse every no-email caller onto one identity.
//
// Composition shape: gatewayAuth → subjectRateLimit → mcp transport.
// requireAuth stays the gate for /me/install; gatewayAuth is a sibling
// that differs only in the 401 envelope.
func (s *Server) gatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			// RFC 6750 §3.1 defines only three error codes for the
			// Bearer challenge: invalid_request, invalid_token,
			// insufficient_scope. "missing bearer" maps to
			// invalid_request (the request is missing a required
			// parameter — the Authorization header).
			s.writeMCPAuthChallenge(w, "invalid_request", "Authorization: Bearer <id_token> required")
			return
		}
		claims, err := s.verifier.VerifyIDToken(r.Context(), raw)
		if err != nil {
			s.log.WarnContext(r.Context(), "broker: mcp gateway id_token verification failed", "err", err)
			s.writeMCPAuthChallenge(w, "invalid_token", "invalid bearer token")
			return
		}
		if !oidcDomainAllowed(s.cfg.OIDC.AllowDomains, claims.HD) {
			s.log.InfoContext(r.Context(), "broker: mcp gateway bearer rejected by allowDomains",
				"subject", hashSubject(claims.Subject), "hd", claims.HD)
			s.writeMCPAuthChallenge(w, "invalid_token", "invalid bearer token")
			return
		}
		if !claims.EmailVerified {
			s.log.WarnContext(r.Context(), "broker: mcp gateway rejected unverified email",
				"subject", hashSubject(claims.Subject))
			s.writeMCPAuthChallenge(w, "invalid_token", "email not verified")
			return
		}
		if canonicalEmail(claims.Email) == "" {
			s.log.WarnContext(r.Context(), "broker: mcp gateway rejected empty email claim",
				"subject", hashSubject(claims.Subject))
			s.writeMCPAuthChallenge(w, "invalid_token", "email claim missing")
			return
		}
		next.ServeHTTP(w, r.WithContext(ctxWithClaims(r.Context(), &claims)))
	})
}

// canonicalEmail is the shared identity normalization: trim
// surrounding whitespace and lowercase. Used by gatewayAuth (to gate
// empty-email claims) and the write gate (WorldConfig.Allow matching).
// Pulled out so both call sites share one definition and a future
// tweak (e.g. Unicode normalization) lands in one place.
func canonicalEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// writeMCPAuthChallenge writes the 401 response shape for MCP clients.
// The WWW-Authenticate header pairs RFC 6750's Bearer scheme (with
// `error=` for the standard sub-codes) with RFC 9728's
// `resource_metadata` parameter pointing at the broker's protected-
// resource document. A compliant MCP client follows the
// resource_metadata link, reads `authorization_servers`, fetches the
// authorization-server metadata, and initiates the OAuth dance — no
// hard-coded broker URL required on the client side.
func (s *Server) writeMCPAuthChallenge(w http.ResponseWriter, errCode, body string) {
	// Built from the gateway's own host — the metadata path only
	// exists on the MCP listener, not the issuer host.
	metadataURL := s.cfg.Server.MCP.PublicURL + prmPath
	challenge := fmt.Sprintf(`Bearer realm="demarkus-broker", error=%q, resource_metadata=%q`, errCode, metadataURL)
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, body, http.StatusUnauthorized)
}
