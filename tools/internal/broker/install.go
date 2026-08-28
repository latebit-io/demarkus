package broker

import (
	"net/http"
	"strings"
)

// installResponse confirms identity and lists readable worlds without
// returning credentials. Publish tokens remain broker-internal.
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

// meInstall returns identity and readable, installable worlds without
// writing Secrets. No installable PublicURL produces 200 with worlds: [].
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
		var listErr error
		out, listErr = installableWorlds(s.cfg, claims, s.tenantScopedInstall())
		if listErr != nil {
			// Fail closed with an empty listing, but never silently:
			// an ambiguous tenant mapping is an operator problem.
			s.log.WarnContext(r.Context(), "broker: install world listing denied",
				"subject", hashSubject(claims.Subject), "err", listErr)
		}
	}
	s.log.InfoContext(r.Context(), "broker: /me/install succeeded",
		"subject", hashSubject(claims.Subject), "worlds", len(out))
	writeJSON(w, http.StatusOK, installResponse{
		Email:  claims.Email,
		Worlds: out,
	})
}

// tenantScopedInstall reports whether management-API world listings are
// tenant-scoped, from the same profile that scopes the gateway.
func (s *Server) tenantScopedInstall() bool {
	return s.profile != nil && s.profile.TenantScoped
}

// installableWorlds lists the worlds a client can be wired at (no
// PublicURL = uninstallable); tenant scoping uses the canonical
// resolver, denying closed with an error the caller logs.
func installableWorlds(cfg *Config, claims *Claims, tenantScoped bool) ([]installWorld, error) {
	var worlds []WorldConfig
	if tenantScoped {
		w, err := tenantWorldFor(cfg, claims)
		if err != nil {
			return []installWorld{}, err
		}
		worlds = []WorldConfig{w}
	} else {
		worlds = readableWorlds(cfg)
	}
	out := make([]installWorld, 0, len(worlds))
	for j := range worlds {
		if worlds[j].PublicURL == "" {
			continue
		}
		out = append(out, installWorld{
			Name:      worlds[j].Name,
			PublicURL: worlds[j].PublicURL,
		})
	}
	return out, nil
}
