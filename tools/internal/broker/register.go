package broker

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// maxRegisterBodySize bounds how much JSON the broker will buffer from
// a /register request. RFC 7591 client metadata is typically small (a
// few hundred bytes — redirect_uris, client_name, grant_types). 64 KiB
// stops a misbehaving or hostile client from forcing the broker to
// allocate unbounded memory while still admitting any realistic
// registration payload.
const maxRegisterBodySize = 64 << 10

// clientIDByteLen: 128-bit client_id entropy — collision-resistant
// (registrations persist keyed on it) yet short enough for log lines.
const clientIDByteLen = 16

// register implements RFC 7591 Dynamic Client Registration (MCP SDKs
// require a registration_endpoint). redirect_uris are validated and
// persisted so authorize can trust an MCP host's https callback.
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRegisterBodySize+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":             "invalid_client_metadata",
			"error_description": "could not read request body",
		})
		return
	}
	if len(body) > maxRegisterBodySize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error":             "invalid_client_metadata",
			"error_description": "request body exceeds size limit",
		})
		return
	}
	// Decode into a generic map so unknown RFC 7591 + RFC 7592 fields
	// (software_id, software_version, jwks, contacts, …) round-trip
	// through without us tracking each one. RFC 7591 §3.1 lets the
	// metadata object itself be empty (`{}`); we extend that to empty
	// bodies as a permissive convenience — semantically equivalent and
	// strictly easier on clients that forget the trailing braces.
	doc := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &doc); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_client_metadata",
				"error_description": "request body is not valid JSON",
			})
			return
		}
	}
	clientID, err := newClientID()
	if err != nil {
		// crypto/rand failure is a process-level condition (entropy
		// pool exhausted on a misconfigured kernel); surface 500 so
		// the operator notices rather than papering over with a
		// degraded id.
		s.log.ErrorContext(r.Context(), "broker: register: client_id generation failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Broker-issued fields win over caller-supplied values:
	// auth-method pinned to "none" (PKCE-only, no client_secret) and
	// grant_types to what the token endpoint actually accepts.
	doc["client_id"] = clientID
	doc["client_id_issued_at"] = s.clock().Unix()
	doc["token_endpoint_auth_method"] = "none"
	doc["grant_types"] = []string{"authorization_code", "urn:ietf:params:oauth:grant-type:device_code"}

	// redirect_uris, when supplied, are validated and PERSISTED: the
	// authorize leg trusts non-loopback redirects only for a client_id
	// whose registration records them (the MCP-host path).
	if uris, ok := doc["redirect_uris"]; ok {
		redirectURIs, parseErr := parseRedirectURIs(uris)
		if parseErr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_redirect_uri",
				"error_description": parseErr.Error(),
			})
			return
		}
		name := ""
		if rawName, present := doc["client_name"]; present {
			var isString bool
			if name, isString = rawName.(string); !isString {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error":             "invalid_client_metadata",
					"error_description": "client_name must be a string",
				})
				return
			}
		}
		if regErr := s.dynamicClients.Register(r.Context(), clientID, redirectURIs, name); regErr != nil {
			s.log.ErrorContext(r.Context(), "broker: register: persist registration failed", "err", regErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error":             "invalid_client_metadata",
				"error_description": "registration could not be persisted; try again",
			})
			return
		}
	}
	// Cache-Control: no-store mirrors the OAuth token-endpoint
	// posture from RFC 6749 §5.1 — the response carries a freshly
	// minted credential-shaped identifier and should never be cached
	// by intermediaries. RFC 7591 doesn't mandate it, but
	// credential-issuing endpoint hygiene says set it anyway.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, doc)
}

// parseRedirectURIs decodes and validates RFC 7591 redirect_uris:
// a non-empty JSON string array whose entries are https URLs or http
// loopback, with no userinfo and no fragment.
func parseRedirectURIs(raw any) ([]string, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("redirect_uris must be a non-empty array of strings")
	}
	uris := make([]string, 0, len(list))
	for _, entry := range list {
		u, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("redirect_uris must be a non-empty array of strings")
		}
		if err := validateClientRedirectURI(u); err != nil {
			return nil, err
		}
		uris = append(uris, u)
	}
	return uris, nil
}

// newClientID returns a fresh base64url-encoded random client_id. The
// encoding choice matches the rest of the broker's token surface
// (device_code, user_code are also base64url-ish) so logs stay
// uniform. Unpadded because RFC 7591 treats client_id as an opaque
// string and operators reading logs benefit from the shorter form.
func newClientID() (string, error) {
	buf := make([]byte, clientIDByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
