package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// gatewayAuth is the gateway's bearer gate: the bearer check every surface
// shares, with the RFC 6750 and RFC 9728 challenge a compliant MCP client
// discovers the OAuth flow from.
func (g *Gateway) gatewayAuth(next http.Handler) http.Handler {
	check := core.BearerCheck{Verifier: g.deps.Verifier, AllowDomains: g.deps.AllowDomains}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := core.BearerToken(r)
		if raw == "" {
			// RFC 6750 3.1: a missing Authorization header is invalid_request.
			g.writeMCPAuthChallenge(w, "invalid_request", "Authorization: Bearer <id_token> required")
			return
		}
		claims, err := check.Authenticate(r.Context(), g.resource, raw)
		switch {
		case err == nil:
			next.ServeHTTP(w, r.WithContext(core.CtxWithClaims(r.Context(), &claims)))
		case errors.Is(err, core.ErrBoundElsewhere):
			// RFC 8707: a token bound to the other gateway's resource stops here.
			g.deps.Log.InfoContext(r.Context(), "broker: mcp gateway token bound to another resource",
				"subject", core.HashSubject(claims.Subject), "resource", claims.Resource)
			g.writeMCPAuthChallenge(w, "invalid_token", "token is bound to another resource")
		case errors.Is(err, core.ErrIdentityUnverified), errors.Is(err, core.ErrIdentityDomain), errors.Is(err, core.ErrIdentityNoEmail):
			g.deps.Log.InfoContext(r.Context(), "broker: mcp gateway identity rejected", "err", err,
				"subject", core.HashSubject(claims.Subject), "hd", claims.HD)
			description := strings.TrimPrefix(err.Error(), "broker: ")
			if errors.Is(err, core.ErrIdentityDomain) {
				// Do not tell a foreign identity which domains are admitted.
				description = "invalid bearer token"
			}
			g.writeMCPAuthChallenge(w, "invalid_token", description)
		default:
			g.deps.Log.WarnContext(r.Context(), "broker: mcp gateway id_token verification failed", "err", err)
			g.writeMCPAuthChallenge(w, "invalid_token", "invalid bearer token")
		}
	})
}

// writeMCPAuthChallenge writes the MCP 401: an RFC 6750 Bearer challenge
// carrying RFC 9728's resource_metadata link, from which a compliant client
// discovers the authorization server with no broker URL configured.
func (g *Gateway) writeMCPAuthChallenge(w http.ResponseWriter, errCode, body string) {
	// Built from the gateway's own host: the metadata path only exists on
	// the MCP listener, not the issuer host.
	metadataURL := g.deps.Gateway.PublicURL + prmPath
	realm := g.deps.Realm
	if realm == "" {
		realm = g.profile.ServerName
	}
	challenge := fmt.Sprintf(`Bearer realm=%q, error=%q, resource_metadata=%q`, realm, errCode, metadataURL)
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, body, http.StatusUnauthorized)
}
