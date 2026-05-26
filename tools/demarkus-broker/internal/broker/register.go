package broker

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

// clientIDByteLen is the entropy budget for the issued client_id. 16
// bytes (128 bits) is the IETF's secure-random recommendation for
// opaque identifiers — collision-resistant against any realistic
// caller volume and short enough that the base64url-encoded form fits
// inside a single log line. The broker does not persist these, so the
// only constraint is that two concurrent /register calls never
// collide.
const clientIDByteLen = 16

// registerHandler implements RFC 7591 Dynamic Client Registration.
//
// Why this exists: MCP clients (the model-context-protocol SDK that
// Claude Code uses to talk to the broker's /mcp gateway) refuse to
// proceed against an authorization server whose discovery doc omits
// `registration_endpoint` — the SDK is built to RFC 7591 + MCP-auth-
// spec assumptions and short-circuits with "Incompatible auth server"
// when the field is missing. Surfacing /register makes the broker a
// well-formed MCP authorization server.
//
// What this is NOT: a client database. The broker has no read of
// `client_id` anywhere on the device-flow path — device.go:108 accepts
// any non-empty value, no lookup, no policy. Real access control lives
// at the id_token verification + per-world `Allow` list. So /register
// rubber-stamps any caller: mints a random `client_id`, echoes the
// request metadata, returns. No persistence, no validation beyond
// shape. If the broker ever grows client_id-level enforcement
// (per-client audit, registered redirect_uri checks, rate buckets keyed
// on client_id), THAT is the moment to add storage and replace this
// handler — adding it speculatively now would be vestigial state.
//
// Per RFC 7591:
//   - §3.1 request is a JSON object with client metadata
//   - §3.2.1 success response is 201 Created with the same JSON object
//     augmented with `client_id` (required) and `client_id_issued_at`
//     (recommended)
//   - §3.2.2 error response is 400 Bad Request with
//     `error: invalid_client_metadata` (we only surface this for
//     malformed JSON; everything else is accepted)
//
// Authentication: none. The endpoint is anonymous-open (RFC 7591 §2 —
// "open registration"). The broker's IP rate limiter (ipRateLimit
// middleware in server.go) caps the per-IP /register rate so a single
// host can't flood the endpoint; cluster-wide rate limiting is a
// future enhancement if the open posture turns out to be exploitable
// at scale.
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
	// Broker-issued fields win over any caller-supplied client_id /
	// client_id_issued_at in the request body. token_endpoint_auth_method
	// is pinned to "none" because the broker's device-flow path is
	// PKCE-only (no client_secret); echoing back the caller's value
	// would let a misconfigured client think it needs a secret it
	// will never get.
	//
	// grant_types is force-pinned to device_code for the same reason
	// the broker pins token_endpoint_auth_method: it's the only grant
	// type the token endpoint actually accepts. Echoing the caller's
	// requested grant_types (e.g. ["authorization_code"]) would
	// silently rubber-stamp a registration whose downstream auth
	// dance is guaranteed to fail at /device/token. RFC 7591 §3.2.2
	// further says the AS MUST respond with invalid_client_metadata
	// when it does not allow a requested grant type; here we
	// normalize rather than reject to keep the spec-following
	// authorization_code-first MCP clients moving — they get a clear
	// signal in the response about what the broker actually supports.
	doc["client_id"] = clientID
	doc["client_id_issued_at"] = s.clock().Unix()
	doc["token_endpoint_auth_method"] = "none"
	doc["grant_types"] = []string{"urn:ietf:params:oauth:grant-type:device_code"}
	// Cache-Control: no-store mirrors the OAuth token-endpoint
	// posture from RFC 6749 §5.1 — the response carries a freshly
	// minted credential-shaped identifier and should never be cached
	// by intermediaries. RFC 7591 doesn't mandate it, but
	// credential-issuing endpoint hygiene says set it anyway.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, doc)
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
