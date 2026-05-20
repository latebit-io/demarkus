package broker

import (
	"errors"
	"net/http"
	"time"
)

// installResponse is the JSON shape returned by GET /me/install. The
// field names — name, publicURL, label, accessToken, expiresAt — are the
// wire contract shared with tools/demarkus-join (PR6) and the plugin
// slash commands (PR7); keep them stable.
//
// PartialFailure is omitted when zero or all worlds minted successfully.
// When a subset succeeded, it carries a stable client-safe code (full
// error detail stays in the broker log) so the consumer can surface a
// "your install completed for some worlds but not others — try again"
// message without parsing free-form error text.
type installResponse struct {
	Worlds         []installWorld `json:"worlds"`
	PartialFailure string         `json:"partialFailure,omitempty"`
}

// installWorld is one row of the install bundle. PublicURL is always
// populated (worlds without a PublicURL are filtered out before mint
// by the MintFiltered predicate in meInstall — see install.go's
// filtering comment). Label is included so /tokens/{label}/rotate and
// DELETE /tokens/{label} are actionable from the install bundle
// without a second round trip to GET /tokens.
type installWorld struct {
	Name        string    `json:"name"`
	PublicURL   string    `json:"publicURL"`
	Label       string    `json:"label"`
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// meInstall handles GET /me/install. Authenticated via requireAuth
// upstream; rate-limited per-subject by subjectRateLimit. The endpoint
// returns the caller's full install bundle — one entry per world the
// identity is authorized for AND that has a PublicURL configured.
//
// Side effect: every call MINTS A FRESH ACCESS TOKEN per returned
// world. Raw token material is never recoverable from the issuances
// Secret (only the hash lives on disk), so there is no reuse path —
// the alternative would be "no token returned" which defeats the
// purpose of /me/install. Old tokens stay valid until ExpiresAt; the
// Sweeper retires them. GET (vs POST) matches the OAuth /me-style
// convention; the mint side-effect is the price of the convention,
// not a sign of REST-idempotent semantics.
//
// Response shape:
//
//	200 OK
//	Content-Type: application/json
//	Cache-Control: no-store
//	Pragma: no-cache
//	{
//	  "worlds": [
//	    {
//	      "name": "team-a",
//	      "publicURL": "mark://world-a.example:6309",
//	      "label": "usr_b23fbc20",
//	      "accessToken": "<raw token>",
//	      "expiresAt": "2026-05-16T18:00:00Z"
//	    }
//	  ]
//	}
//
// Empty-worlds policy: an authenticated identity with zero authorized
// worlds (or with all authorized worlds filtered by the PublicURL
// predicate) returns 200 + worlds: []. NOT 403. The user IS
// authenticated; the absence of installable worlds is an authz-config
// emptiness story, not an auth-failure story. The plugin layer needs
// to distinguish these so it can surface a "no worlds authorized for
// your identity" message rather than retrying auth.
//
// Partial failure: when some worlds minted successfully but a later
// one failed (k8s API blip, RBAC denial on one world's Secret, etc.),
// returns 200 + the successful worlds + a "partialFailure" code.
// Matches /auth/callback's shape for the same condition so consumers
// have one error model across the two mint paths.
func (s *Server) meInstall(w http.ResponseWriter, r *http.Request) {
	claims, ok := claimsFromCtx(r.Context())
	if !ok {
		// Programming error: route registered without requireAuth
		// upstream. Fail closed with 500 so a future route-table
		// edit that drops requireAuth surfaces immediately rather
		// than serving a bundle to anonymous callers.
		s.log.ErrorContext(r.Context(), "broker: meInstall missing claims in context — middleware miswired")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Filter at mint time, not after. A world without a PublicURL is
	// structurally un-installable (the plugin has no address to wire
	// the client at) — minting a token for it would write an
	// issuance record we'd then discard from the response, churning
	// the issuances Secret toward its 5000-record cap for zero
	// behavioral gain. MintFiltered drops the world before any
	// Secret is touched.
	keep := func(world *WorldConfig) bool { return world.PublicURL != "" }
	results, mintErr := s.issuer.MintFiltered(r.Context(), claims, keep)

	// no-store + no-cache: response body carries raw access tokens
	// that must not be cached by intermediaries, browser disk caches,
	// or proxies. Set BEFORE WriteHeader (writeJSON flushes headers)
	// so the directives actually land on the wire.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	if mintErr != nil {
		// Partial mint: some worlds succeeded before a later one
		// failed. Same shape as /auth/callback so consumers see one
		// error model across the two mint paths.
		if len(results) > 0 {
			s.log.WarnContext(r.Context(), "broker: /me/install partial mint",
				"err", mintErr, "subject", hashSubject(claims.Subject), "minted", len(results))
			writeJSON(w, http.StatusOK, installResponse{
				Worlds:         s.toInstallWorlds(results),
				PartialFailure: "one_or_more_worlds_failed",
			})
			return
		}
		if errors.Is(mintErr, ErrNotAuthorized) {
			// Authenticated identity with zero authorized worlds (or
			// every authorized world filtered by the PublicURL
			// predicate). 200 + worlds: [] — see the empty-worlds
			// policy in the function doc comment.
			s.log.InfoContext(r.Context(), "broker: /me/install no installable worlds for identity",
				"subject", hashSubject(claims.Subject))
			writeJSON(w, http.StatusOK, installResponse{Worlds: []installWorld{}})
			return
		}
		if errors.Is(mintErr, ErrEmailUnverified) {
			// Defense in depth. The production Verifier rejects
			// unverified identities at the bearer-validation layer
			// (requireAuth), so this branch is structurally
			// unreachable in production wiring — but a future
			// Verifier impl or a test double might admit an
			// unverified identity, and the cost of an unverified
			// install bundle slipping through is the same as an
			// unverified /auth/callback mint.
			s.log.InfoContext(r.Context(), "broker: /me/install rejected unverified identity",
				"subject", hashSubject(claims.Subject))
			http.Error(w, "email not verified", http.StatusForbidden)
			return
		}
		s.log.ErrorContext(r.Context(), "broker: /me/install mint failed",
			"err", mintErr, "subject", hashSubject(claims.Subject))
		http.Error(w, "mint failed", http.StatusInternalServerError)
		return
	}
	s.log.InfoContext(r.Context(), "broker: /me/install succeeded",
		"subject", hashSubject(claims.Subject), "worlds", len(results))
	writeJSON(w, http.StatusOK, installResponse{Worlds: s.toInstallWorlds(results)})
}

// toInstallWorlds joins each MintResult with its world's PublicURL by
// walking s.cfg.Worlds. O(N*M) with N = mint results, M = configured
// worlds; both are bounded to dozens in realistic deployments and the
// path is called at most once per session per user.
//
// MintFiltered's predicate already excluded worlds without a
// PublicURL, so the lookup is guaranteed to find a non-empty value for
// every result. A world that vanished from config between
// MintFiltered's iteration and this join (an operator hot-reload
// removing a world mid-request) would surface as an empty PublicURL;
// that's a config-time race rather than a runtime fault, and we
// prefer "publish the row with an empty publicURL" over hiding it —
// the consumer (tools/demarkus-join) skips empty entries; the user
// sees a "no worlds installed for X" message; logs and metrics keep
// the partial-failure trail. The alternative would be a second mint-
// path failure leg and a more confusing partial-failure response.
func (s *Server) toInstallWorlds(results []MintResult) []installWorld {
	out := make([]installWorld, 0, len(results))
	for _, r := range results {
		out = append(out, installWorld{
			Name:        r.World,
			PublicURL:   s.lookupPublicURL(r.World),
			Label:       r.Label,
			AccessToken: r.RawToken,
			ExpiresAt:   r.ExpiresAt,
		})
	}
	return out
}

// lookupPublicURL returns the configured PublicURL for the named world
// or "" if the world is not in cfg.Worlds. See toInstallWorlds for the
// empty-result handling rationale.
func (s *Server) lookupPublicURL(name string) string {
	for j := range s.cfg.Worlds {
		if s.cfg.Worlds[j].Name == name {
			return s.cfg.Worlds[j].PublicURL
		}
	}
	return ""
}
