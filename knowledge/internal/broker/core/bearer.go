package core

import (
	"net/http"
	"strings"
)

// BearerToken is the Authorization bearer, or "" when absent or malformed.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	// RFC 6750 §2.1: the scheme is case-insensitive. Clients in the wild
	// send "Bearer", "bearer", and even "BEARER"; accept all of them.
	parts := strings.Fields(h)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}
