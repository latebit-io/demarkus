// Package token provides the shared mint primitives used by demarkus-token
// (CLI) and demarkus-broker (OIDC token issuer). Behavior is split so callers
// can compose freely:
//
//   - Generate is pure: it mints a fresh raw token, hashes it, and returns
//     both the raw secret (one-time output to the user) and the persistent
//     Entry (hash + capabilities). It performs no I/O.
//
//   - The TOML helpers in this package (ReadFile, AppendEntry, WriteFile)
//     handle on-disk persistence for the CLI path. The broker uses Generate
//     directly and writes the Entry into a Kubernetes Secret — no TOML file
//     involvement.
//
// The hash format is fixed by protocol.HashToken, the single source of truth
// shared with the server.
package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/latebit/demarkus/protocol"
)

// Entry is the persistent shape of a single token as the server reads it
// from tokens.toml. The struct tags match the on-disk format exactly.
type Entry struct {
	Hash       string   `toml:"hash"`
	Paths      []string `toml:"paths"`
	Operations []string `toml:"operations"`
	Expires    string   `toml:"expires,omitempty"`
}

// Minted is the result of generating a fresh token. Raw is the one-time
// secret to hand to the caller; Entry is what gets persisted (and crucially
// does not contain Raw — the server only ever sees the hash).
type Minted struct {
	Label string
	Raw   string
	Entry Entry
}

// ErrEmptyLabel is returned by Generate when label is the empty string.
var ErrEmptyLabel = errors.New("label is required")

// Generate mints a new random raw token (32 bytes → 64 hex chars), hashes it
// in the protocol-standard format, and returns the raw token + persistent
// Entry. It performs no I/O — callers persist the Entry however they need
// (TOML file via AppendEntry, k8s Secret via the broker, etc.).
//
// Returns ErrEmptyLabel if label is empty.
func Generate(label string, paths, operations []string) (Minted, error) {
	if label == "" {
		return Minted{}, ErrEmptyLabel
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Minted{}, fmt.Errorf("mint %s: generate random bytes: %w", label, err)
	}
	raw := hex.EncodeToString(secret)
	// Clone the caller's slices so later mutation by the caller cannot
	// silently corrupt Minted.Entry. The broker may build paths/ops from
	// shared config values, and the CLI passes through user-parsed flags —
	// both could be reused after Generate returns.
	return Minted{
		Label: label,
		Raw:   raw,
		Entry: Entry{
			Hash:       protocol.HashToken(raw),
			Paths:      slices.Clone(paths),
			Operations: slices.Clone(operations),
		},
	}, nil
}
