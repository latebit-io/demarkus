package broker

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// clientAuth is the client identity a token-endpoint request presented,
// extracted from HTTP Basic (RFC 6749 §2.3.1) or client_secret_post
// form parameters. Secret is empty when the client presented no
// credential at all — the public-client shape; whether that is
// acceptable is the caller's decision based on the WebClients registry.
type clientAuth struct {
	ClientID string
	Secret   string
}

// parseClientAuth extracts the client credentials from a token-endpoint
// request. Precedence and error rules follow RFC 6749 §2.3:
//
//   - HTTP Basic wins when present. Username and password are
//     form-urlencoded before Basic-encoding per §2.3.1, so both halves
//     are unescaped here. A form client_id that contradicts the Basic
//     username, or a form client_secret alongside Basic, is "more than
//     one authentication mechanism" and is rejected.
//   - Otherwise client_id + client_secret come from the form
//     (client_secret_post). Either may be empty — a bare client_id
//     with no secret is the existing public-client request shape.
//
// Errors are client mistakes; callers map them to invalid_request.
func parseClientAuth(r *http.Request) (clientAuth, error) {
	basicID, basicSecret, ok := r.BasicAuth()
	if !ok {
		return clientAuth{
			ClientID: r.PostFormValue("client_id"),
			Secret:   r.PostFormValue("client_secret"),
		}, nil
	}
	if r.PostFormValue("client_secret") != "" {
		return clientAuth{}, errors.New("client must not use both Basic and client_secret_post")
	}
	id, err := url.QueryUnescape(basicID)
	if err != nil {
		return clientAuth{}, fmt.Errorf("basic auth client_id: %w", err)
	}
	secret, err := url.QueryUnescape(basicSecret)
	if err != nil {
		return clientAuth{}, fmt.Errorf("basic auth client_secret: %w", err)
	}
	if formID := r.PostFormValue("client_id"); formID != "" && formID != id {
		return clientAuth{}, errors.New("client_id mismatch between Basic auth and form")
	}
	return clientAuth{ClientID: id, Secret: secret}, nil
}

// verifyWebClientSecret reports whether the presented plaintext secret
// matches the client's registered sha256 hash. The compare runs over
// the two hex digests in constant time; hashing the presented secret
// first means the comparison length is fixed regardless of what the
// client sent, so neither length nor content leaks through timing.
func verifyWebClientSecret(wc *WebClientConfig, secret string) bool {
	if secret == "" {
		return false
	}
	sum := sha256.Sum256([]byte(secret))
	presented := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(presented), []byte(wc.ClientSecretHash)) == 1
}

// writeInvalidClient emits the RFC 6749 §5.2 invalid_client response:
// 401 with a WWW-Authenticate challenge naming the Basic scheme the
// broker prefers for confidential clients.
func writeInvalidClient(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="demarkus-knowledge-broker"`)
	writeJSON(w, http.StatusUnauthorized, deviceTokenError{Error: "invalid_client"})
}
