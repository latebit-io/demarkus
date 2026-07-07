package broker

import (
	"net/http"
	"strings"
)

// installResponse is the JSON shape returned by GET /me/install. The
// endpoint confirms identity and lists worlds the caller is authorized
// to use; no credentials are returned. Per-world demarkus tokens are
// minted lazily inside the broker's MCP gateway on first dispatch and
// are never exposed to clients on the knowledge-system flow (the world
// is unreachable except via the broker, so the client has no use for
// the raw token).
type installResponse struct {
	Email  string         `json:"email"`
	Worlds []installWorld `json:"worlds"`
}

// installWorld is one row of the install bundle. PublicURL is the
// caller-facing address of the world's broker gateway; worlds without
// one are omitted from the response.
type installWorld struct {
	Name      string `json:"name"`
	PublicURL string `json:"publicURL"`
}

// meInstall handles GET /me/install. Authenticated via requireAuth
// upstream; rate-limited per-subject by subjectRateLimit. The endpoint
// returns the caller's identity and the worlds it is authorized for —
// no Secret writes, no token material, idempotent across calls.
//
// Empty-worlds policy: an authenticated identity with zero authorized
// worlds (or with all authorized worlds filtered out for lacking a
// PublicURL) returns 200 + worlds: []. NOT 403. The identity IS
// authenticated; the absence of installable worlds is an authz-config
// emptiness story, not an auth-failure story. The plugin layer needs
// to distinguish these so it can surface a "no worlds authorized for
// your identity" message rather than retrying auth.
func (s *Server) meInstall(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromCtx(r.Context())
	if !ok {
		// Programming error: route registered without requireAuth
		// upstream. Fail closed with 500 so a future route-table
		// edit that drops requireAuth surfaces immediately rather
		// than serving a bundle to anonymous callers.
		s.log.ErrorContext(r.Context(), "broker: meInstall missing claims in context; middleware miswired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Defense in depth: requireAuth's bearer-validation layer is the
	// primary gate against unverified identities. Catch it again here
	// so a future Verifier impl or test double that admits an
	// unverified identity does not slip an install bundle past.
	if !claims.EmailVerified {
		s.log.InfoContext(r.Context(), "broker: /me/install rejected unverified identity",
			"subject", hashSubject(claims.Subject))
		http.Error(w, "email not verified", http.StatusForbidden)
		return
	}
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))

	// no-store + no-cache: today the body carries only identity + URLs,
	// but the headers are set before writeJSON so a future addition of
	// any credential material to the response inherits the same posture
	// without having to re-discover the OAuth2 §5.1 no-store rule.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	out := []installWorld{}
	if claims.Email != "" {
		for _, world := range authorizedWorlds(s.cfg, claims) {
			// A world without a PublicURL is not installable: the
			// plugin has no address to wire the client at. Skip it
			// rather than report it and force the consumer to
			// re-filter.
			if world.PublicURL == "" {
				continue
			}
			out = append(out, installWorld{
				Name:      world.Name,
				PublicURL: world.PublicURL,
			})
		}
	}
	s.log.InfoContext(r.Context(), "broker: /me/install succeeded",
		"subject", hashSubject(claims.Subject), "worlds", len(out))
	writeJSON(w, http.StatusOK, installResponse{
		Email:  claims.Email,
		Worlds: out,
	})
}
