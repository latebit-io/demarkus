package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/latebit/demarkus/protocol/token"
	"k8s.io/client-go/kubernetes"
)

// worldWriteTokenSecretKey is the data key in the broker's
// per-world Secret that holds the JSON-encoded write token entry.
const worldWriteTokenSecretKey = "write-token.json"

// worldWriteTokenSecretName derives the broker-namespace Secret
// name holding the canonical write token for a configured world.
// Per-world (not one shared blob) so operators can grant per-world
// RBAC if some worlds need stricter access than others.
func worldWriteTokenSecretName(worldName string) string {
	return "demarkus-broker-write-token-" + worldName
}

// worldWriteTokenLabel is the stable label the broker uses for its
// entry in the world's tokens.toml. Encoding the world name makes
// the entry obvious to an operator inspecting the world Secret;
// keeping it stable means re-provisioning hits the same entry
// instead of accumulating dead labels.
func worldWriteTokenLabel(worldName string) string {
	return "broker-write-" + worldName
}

// writeTokenRecord is the JSON shape persisted in the broker's
// per-world write-token Secret. The raw token is at rest here so
// any broker pod can recover it and dispatch writes consistently;
// the world Secret only ever sees the hash (Entry.Hash).
type writeTokenRecord struct {
	Label    string      `json:"label"`
	RawToken string      `json:"rawToken"`
	Entry    token.Entry `json:"entry"`
}

// worldWriteTokenStore provisions and caches one long-lived write
// token per configured world. The token's hash is appended to the
// world's tokens.toml; the raw token is kept in a broker-namespace
// per-world Secret so every broker pod (and pod restarts) converge
// on the same value rather than churning the world's tokens Secret.
//
// Per-user mint and per-(email, world) caches are intentionally
// not part of this design: SSO at the broker is the org gate,
// WorldConfig.Allow is the writer allowlist, and the broker is the
// only party that ever holds the raw token.
type worldWriteTokenStore struct {
	cfg   *Config
	k8s   kubernetes.Interface
	clock func() time.Time

	mu    sync.Mutex
	cache map[string]string // worldName → raw token
}

func newWorldWriteTokenStore(cfg *Config, k8s kubernetes.Interface) *worldWriteTokenStore {
	return &worldWriteTokenStore{
		cfg:   cfg,
		k8s:   k8s,
		clock: time.Now,
		cache: make(map[string]string),
	}
}

// Get returns the cached raw write token for worldName, or ok=false
// if this broker pod has not provisioned the world yet. Callers
// that need the token MUST fall through to Provision on miss —
// Get is a fast-path read for hot dispatch loops.
func (s *worldWriteTokenStore) Get(worldName string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.cache[worldName]
	return tok, ok
}

// Provision returns the raw write token for worldName, minting and
// persisting one if no broker pod has done it yet. Idempotent and
// safe across concurrent broker pods: the broker-namespace
// per-world Secret holds the canonical record, and mutateSecret's
// resourceVersion-based optimistic concurrency converges
// concurrent provisioners on the first commit's token.
//
// After the broker Secret holds a token, Provision ensures the
// world's tokens.toml has the matching hash under the stable
// label. The world write is idempotent for the same label+hash
// pair, so a re-provision after a partial failure converges
// without leaving an orphan entry.
func (s *worldWriteTokenStore) Provision(ctx context.Context, worldName string) (string, error) {
	if tok, ok := s.Get(worldName); ok {
		return tok, nil
	}
	world := s.lookupWorld(worldName)
	if world == nil {
		// Same sentinel the worldPool returns so callers
		// (notably federation/graph) can treat "no broker config
		// for this world" identically across the dispatch and
		// provision layers — see mcp_tools_federation.go's
		// "not a knowledge-system world" mapping.
		return "", &errWorldNotFound{worldName: worldName}
	}

	label := worldWriteTokenLabel(worldName)
	secretName := worldWriteTokenSecretName(worldName)

	// mutateSecret races concurrent broker pods on the broker Secret.
	// On a fresh world: the closure mints once and the encoded
	// record lands. On a re-provision (or a loser of the race): the
	// closure sees existing data and returns it unchanged so the
	// already-committed token wins.
	var finalRecord writeTokenRecord
	err := mutateSecret(ctx, s.k8s, s.cfg.Server.BrokerNamespace, secretName, worldWriteTokenSecretKey, func(existing []byte) ([]byte, error) {
		if len(existing) > 0 {
			if err := json.Unmarshal(existing, &finalRecord); err != nil {
				return nil, fmt.Errorf("decode write token record: %w", err)
			}
			return existing, nil
		}
		// Operations is hardcoded — NOT sourced from
		// world.DefaultToken.Operations — to keep the open-reads
		// design impossible to regress via config. The server only
		// recognizes "publish" for write verbs (PUBLISH / APPEND /
		// ARCHIVE all call Authorize with "publish"), so anything
		// else is dead weight. Critically, if "read" ever leaked
		// into the operator's `operations:` list, the server's
		// `RequiresReadAuth` would flip on for every path matched
		// by this token (`/*` in the default config) and every
		// open-bearer read would start returning unauthorized.
		// One char of config drift would silently break the
		// architectural invariant the entire broker simplification
		// rests on. Path stays configurable — narrowing the write
		// surface to e.g. /team-a/** is a legitimate operator knob.
		minted, mErr := token.Generate(label, world.DefaultToken.Paths, []string{"publish"})
		if mErr != nil {
			return nil, fmt.Errorf("generate write token: %w", mErr)
		}
		// Long-lived: explicitly leave Expires empty. The world's
		// tokens.toml treats an empty Expires as "no expiry" which
		// is the contract this design depends on.
		minted.Entry.Expires = ""
		finalRecord = writeTokenRecord{Label: label, RawToken: minted.Raw, Entry: minted.Entry}
		encoded, encErr := json.Marshal(finalRecord)
		if encErr != nil {
			return nil, fmt.Errorf("encode write token record: %w", encErr)
		}
		return encoded, nil
	})
	if err != nil {
		return "", err
	}

	// Sync the world's tokens.toml with the canonical entry.
	// AppendBytes returns ErrLabelExists when the label is already
	// present, which is the steady-state on re-provision — treat
	// it as success since the existing hash matches our raw token.
	if syncErr := s.syncWorldHash(ctx, world, finalRecord.Label, &finalRecord.Entry); syncErr != nil {
		return "", syncErr
	}

	s.mu.Lock()
	s.cache[worldName] = finalRecord.RawToken
	s.mu.Unlock()
	return finalRecord.RawToken, nil
}

// syncWorldHash brings the world's tokens.toml in line with the
// broker's canonical write-token entry under the stable label.
// Idempotent for the matching-hash case (the typical state after
// the first provision); reconciling for the mismatched-hash case
// (the broker Secret was recreated under us while the world Secret
// retained the old `broker-write-*` entry). Without the hash check
// the latter case would silently cache a token the world will
// never authorize and every write would burn the propagation-race
// budget pretending kubelet was lagging.
func (s *worldWriteTokenStore) syncWorldHash(ctx context.Context, world *WorldConfig, label string, entry *token.Entry) error {
	return mutateSecret(ctx, s.k8s, world.Namespace, world.TokensSecret, TokensSecretKey, func(existing []byte) ([]byte, error) {
		next, err := token.AppendBytes(existing, label, entry)
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, token.ErrLabelExists) {
			return nil, err
		}
		// Label already present. Compare on-disk hash to decide
		// whether this is the steady-state no-op or a stale entry
		// we need to rewrite.
		current, parseErr := token.ParseBytes(existing)
		if parseErr != nil {
			return nil, fmt.Errorf("parse existing tokens.toml: %w", parseErr)
		}
		existingEntry, ok := current.Tokens[label]
		if !ok {
			// AppendBytes claims the label exists but ParseBytes
			// disagrees. Shouldn't be reachable since both share
			// the same TOML decoder; surface as an error rather
			// than overwrite — the right next step is diagnosing
			// the divergence, not papering over it.
			return nil, fmt.Errorf("syncWorldHash: AppendBytes reported %q exists but ParseBytes did not find it", label)
		}
		if existingEntry.Hash == entry.Hash {
			return existing, nil
		}
		// Hash drift: replace the stale entry so the world
		// recognizes the broker's current canonical token.
		stripped, removeErr := token.RemoveBytes(existing, label)
		if removeErr != nil {
			return nil, fmt.Errorf("remove stale tokens.toml entry %q: %w", label, removeErr)
		}
		rewritten, appendErr := token.AppendBytes(stripped, label, entry)
		if appendErr != nil {
			return nil, fmt.Errorf("rewrite tokens.toml entry %q: %w", label, appendErr)
		}
		return rewritten, nil
	})
}

func (s *worldWriteTokenStore) lookupWorld(name string) *WorldConfig {
	for i := range s.cfg.Worlds {
		if s.cfg.Worlds[i].Name == name {
			return &s.cfg.Worlds[i]
		}
	}
	return nil
}
