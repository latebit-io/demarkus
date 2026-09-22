package broker

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// gatewayAuth is the gateway's bearer gate: the same verification as
// requireAuth, but the 401 carries the RFC 6750 and RFC 9728 challenge a
// compliant MCP client discovers the OAuth flow from.
func (g *mcpGateway) gatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			// RFC 6750 3.1: a missing Authorization header is invalid_request.
			g.writeMCPAuthChallenge(w, "invalid_request", "Authorization: Bearer <id_token> required")
			return
		}
		claims, err := g.deps.Verifier.VerifyIDToken(r.Context(), raw)
		if err != nil {
			g.deps.Log.WarnContext(r.Context(), "broker: mcp gateway id_token verification failed", "err", err)
			g.writeMCPAuthChallenge(w, "invalid_token", "invalid bearer token")
			return
		}
		if err := gateIdentity(g.deps.AllowDomains, &claims); err != nil {
			g.deps.Log.InfoContext(r.Context(), "broker: mcp gateway identity rejected", "err", err,
				"subject", hashSubject(claims.Subject), "hd", claims.HD)
			description := strings.TrimPrefix(err.Error(), "broker: ")
			if errors.Is(err, errIdentityDomain) {
				// Do not tell a foreign identity which domains are admitted.
				description = "invalid bearer token"
			}
			g.writeMCPAuthChallenge(w, "invalid_token", description)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctxWithClaims(r.Context(), &claims)))
	})
}

// writeMCPAuthChallenge writes the MCP 401: an RFC 6750 Bearer challenge
// carrying RFC 9728's resource_metadata link, from which a compliant client
// discovers the authorization server with no broker URL configured.
func (g *mcpGateway) writeMCPAuthChallenge(w http.ResponseWriter, errCode, body string) {
	// Built from the gateway's own host: the metadata path only exists on
	// the MCP listener, not the issuer host.
	metadataURL := g.deps.MCP.PublicURL + prmPath
	realm := g.deps.Realm
	if realm == "" {
		realm = "demarkus-knowledge-broker"
	}
	challenge := fmt.Sprintf(`Bearer realm=%q, error=%q, resource_metadata=%q`, realm, errCode, metadataURL)
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, body, http.StatusUnauthorized)
}
