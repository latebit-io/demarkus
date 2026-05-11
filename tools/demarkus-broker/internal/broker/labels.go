// Package broker implements the demarkus OIDC token broker: a small HTTP
// service that exchanges an OIDC identity for one or more demarkus tokens,
// writing token hashes into per-world Kubernetes Secrets while keeping its
// own issuance state in a separate broker-namespace Secret. World servers
// stay identity-blind; the broker is the only component that knows who
// owns which token.
package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// LabelPrefix is the namespace prefix the broker stamps on every label it
// mints. Admin-minted labels (via the demarkus-token CLI) use any prefix
// the admin chooses; the broker only operates on labels it created itself,
// which it identifies by this prefix plus the issuances Secret.
const LabelPrefix = "usr_"

// NewLabel returns an opaque token label of the form "usr_<8 hex chars>"
// (4 bytes of entropy). Collision space is 2^32, which is fine because the
// issuer rejects duplicates on Secret write (ErrLabelExists) and the
// caller retries on collision.
func NewLabel() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("broker: read random for label: %w", err)
	}
	return LabelPrefix + hex.EncodeToString(b[:]), nil
}
