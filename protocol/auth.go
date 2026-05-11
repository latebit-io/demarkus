package protocol

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashToken returns the SHA-256 hash of a raw token in the format "sha256-<hex>".
//
// This format is a protocol contract: any component that writes a token hash
// (CLI mint tool, OIDC broker) must produce a hash byte-identical to what the
// server reads from its tokens.toml. Centralizing the function here is the
// single source of truth.
//
// The server stores only these hashes. Clients send the raw secret over the
// authenticated transport, and the server hashes before lookup — so the
// tokens file never contains plaintext secrets.
func HashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return "sha256-" + hex.EncodeToString(h[:])
}
