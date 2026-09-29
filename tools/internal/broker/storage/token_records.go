package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/tools/internal/broker/core"
	"github.com/latebit-io/demarkus/tools/internal/token"
)

// TokenRecord is a broker-issued world token at rest in a broker Secret. The
// raw value lives here so every pod converges on it; the world sees the hash.
type TokenRecord struct {
	Label    string      `json:"label"`
	RawToken string      `json:"rawToken"`
	Entry    token.Entry `json:"entry"`
}

// TokenMint is the token EnsureTokenRecord mints when the record is absent.
type TokenMint struct {
	Label string
	Paths []string
}

// EnsureTokenRecord returns the record at ref, minting a publish-only,
// non-expiring token when absent. Concurrent callers converge on the first
// committed record.
func EnsureTokenRecord(ctx context.Context, store core.SecretStore, ref core.SecretRef, mint TokenMint) (TokenRecord, error) {
	raw, _, err := core.EnsureCreated(ctx, store, ref, func() ([]byte, error) {
		// "publish" only, and not configurable: it covers every write verb, and a
		// stray "read" would switch on the server's read auth (AuthorizeRead) for
		// every path this token matches, breaking open reads.
		minted, err := token.Generate(mint.Label, mint.Paths, []string{"publish"})
		if err != nil {
			return nil, fmt.Errorf("generate token %s: %w", mint.Label, err)
		}
		// Empty Expires is "no expiry" in tokens.toml; these tokens are long-lived.
		minted.Entry.Expires = ""
		return json.Marshal(TokenRecord{Label: mint.Label, RawToken: minted.Raw, Entry: minted.Entry})
	})
	if err != nil {
		return TokenRecord{}, err
	}
	return decodeTokenRecord(ref, raw)
}

func decodeTokenRecord(ref core.SecretRef, raw []byte) (TokenRecord, error) {
	var record TokenRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return TokenRecord{}, fmt.Errorf("decode token record %s: %w", ref, err)
	}
	return record, nil
}

// SyncWorldHash makes the world's tokens.toml hold record's entry under its
// label: a no-op when the hash matches, a rewrite when a recreated broker
// Secret left a stale hash behind (the world would reject the canonical token).
func SyncWorldHash(ctx context.Context, store core.SecretStore, world *core.WorldConfig, record *TokenRecord) error {
	label, entry := record.Label, &record.Entry
	return store.Mutate(ctx, core.WorldTokensRef(world), func(existing []byte) ([]byte, error) {
		next, err := token.AppendBytes(existing, label, entry)
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, token.ErrLabelExists) {
			return nil, err
		}
		current, err := token.ParseBytes(existing)
		if err != nil {
			return nil, fmt.Errorf("parse existing tokens.toml: %w", err)
		}
		existingEntry, ok := current.Tokens[label]
		if !ok {
			// Both share one TOML decoder; divergence needs diagnosis, not an overwrite.
			return nil, fmt.Errorf("sync world hash: AppendBytes reported %q exists but ParseBytes did not find it", label)
		}
		if existingEntry.Hash == entry.Hash {
			return existing, nil
		}
		stripped, err := token.RemoveBytes(existing, label)
		if err != nil {
			return nil, fmt.Errorf("remove stale tokens.toml entry %q: %w", label, err)
		}
		rewritten, err := token.AppendBytes(stripped, label, entry)
		if err != nil {
			return nil, fmt.Errorf("rewrite tokens.toml entry %q: %w", label, err)
		}
		return rewritten, nil
	})
}
