// Package broker implements the demarkus OIDC token broker: a small HTTP
// service that gates demarkus world access behind SSO. It provisions and
// holds one long-lived write token per world (hashes land in the world's
// Kubernetes Secret) and serves the MCP gateway; reads dispatch open with
// an empty bearer, writes are gated by WorldConfig.Allow. World servers
// stay identity-blind; the broker is the only component that knows who is
// behind a given call.
package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// LabelPrefix is the prefix the broker stamps on every token label it
// generates, distinguishing broker-generated labels from admin-minted
// ones (via the demarkus-token CLI), which use any prefix the admin
// chooses.
const LabelPrefix = "usr_"

// NewLabel returns an opaque token label of the form "usr_<8 hex chars>"
// (4 bytes of entropy). Collision space is 2^32; callers that persist a
// label are expected to reject duplicates on Secret write and retry on
// collision.
func NewLabel() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("broker: read random for label: %w", err)
	}
	return LabelPrefix + hex.EncodeToString(b[:]), nil
}
