package broker

import "net/http"

// tokenRevoke implements POST /token/revoke per RFC 7009. Drops the
// supplied refresh token from the broker's refreshStore so a future
// /device/token refresh-grant rejects it with invalid_grant.
//
// RFC 7009 semantics this handler implements:
//
//   - §2.1: only `token` is required; `token_type_hint` is optional
//     and may be ignored. The broker accepts the param for spec
//     conformance but does not validate it — refreshStore.Revoke is
//     a sha256 point-lookup, so no hint is needed to find the entry.
//
//   - §2.2: revoking an unknown / already-revoked token MUST NOT
//     return an error. Surfacing 404 or 400 on unknown tokens would
//     turn this endpoint into an enumeration oracle. refreshStore
//     is idempotent by construction.
//
//   - §2.2: response is 200 (or 204). We pick 204 No Content
//     because the body is always empty.
//
// Missing or empty `token` form param is the one case where we
// surface 400 invalid_request — the request itself is malformed,
// which RFC 7009 §2.1 calls out as a separate error class from
// the "unknown token" no-op case.
//
// Anonymous endpoint: no bearer auth required. Possession of the
// token is the authorization. The IP rate limiter is the protection
// against an attacker who knows a token guessing additional ones
// (the sha256 space makes guessing infeasible, but the limiter is
// defense-in-depth same as on /device/authorize).
//
// Note: /token/revoke applies only to refresh tokens minted by
// /device/token. There is no per-user world-token revocation surface —
// the broker provisions one long-lived write token per world, not
// per-user tokens.
func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	rawToken := r.PostFormValue("token")
	if rawToken == "" {
		writeJSON(w, http.StatusBadRequest, deviceTokenError{Error: "invalid_request"})
		return
	}
	// token_type_hint is read but intentionally not validated — the
	// store lookup is by hash and the hint adds no value. Read the
	// form value anyway so the handler is observably aware of the
	// param's existence; a future debug log can pick it up.
	_ = r.PostFormValue("token_type_hint")

	if err := s.refreshStore.Revoke(r.Context(), rawToken); err != nil {
		// refreshStore.Revoke errors are I/O-only — the store layer
		// already absorbs "token unknown" as a no-op. A propagated
		// error here means the Secret read or write failed; surface
		// 500 rather than 204 so the caller knows the revocation
		// did not land. JSON shape matches the rest of the OAuth2
		// surface (RFC 7009 §2.2.1 + RFC 6749 §5.2 — error responses
		// are application/json with an `error` field); the prior
		// text/plain response was the odd one out.
		s.log.ErrorContext(r.Context(), "broker: revoke refresh token failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
