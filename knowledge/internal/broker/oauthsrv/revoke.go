package oauthsrv

import "net/http"

// tokenRevoke is POST /token/revoke (RFC 7009): drops a refresh token so a
// later refresh grant fails. An unknown token answers 204 too (no oracle),
// a missing one 400. Anonymous: possession of the token is the authorization.
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
	// token_type_hint (RFC 7009 §2.1) adds nothing to a lookup by hash;
	// accepted, not validated.
	_ = r.PostFormValue("token_type_hint")

	if err := s.refreshStore.Revoke(r.Context(), rawToken); err != nil {
		// An unknown token is a no-op in the store; an error is I/O, so 500
		// tells the caller the revocation did not land. JSON error shape per
		// RFC 7009 §2.2.1 and RFC 6749 §5.2, like the rest of the surface.
		s.log.ErrorContext(r.Context(), "broker: revoke refresh token failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, deviceTokenError{Error: "server_error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
