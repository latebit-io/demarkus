package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// grantState is the OAuth state that spans requests, one Secret document
// every replica reads and writes: issued authorization codes and device
// grants, keyed by the hash of the code, holding claims and never tokens.
type grantState struct {
	Codes   map[string]codeGrant   `json:"codes,omitempty"`
	Devices map[string]deviceGrant `json:"devices,omitempty"`
}

// codeGrant is an issued authorization code awaiting redemption.
type codeGrant struct {
	ClientID            string      `json:"clientID"`
	RedirectURI         string      `json:"redirectURI"`
	CodeChallenge       string      `json:"codeChallenge"`
	CodeChallengeMethod string      `json:"codeChallengeMethod"`
	Claims              core.Claims `json:"claims"`
	ExpiresAt           time.Time   `json:"expiresAt"`
}

// deviceGrant is one RFC 8628 grant from authorize to the poll that
// collects it; Claims is set once the user signs in.
type deviceGrant struct {
	UserCode     string       `json:"userCode"`
	Status       deviceStatus `json:"status"`
	Resource     string       `json:"resource,omitempty"`
	ExpiresAt    time.Time    `json:"expiresAt"`
	LastPolledAt time.Time    `json:"lastPolledAt,omitzero"`
	Claims       core.Claims  `json:"claims,omitzero"`
}

const (
	// maxCodeGrants and maxDeviceGrants bound each kind; a full store
	// refuses new grants until entries expire, at most one TTL later.
	maxCodeGrants   = 1000
	maxDeviceGrants = 1000
	// maxGrantStateBytes keeps the document under the 1 MiB Secret limit.
	maxGrantStateBytes = 900 << 10
)

// errGrantStoreFull is returned instead of growing the document past a cap.
var errGrantStoreFull = errors.New("broker: too many pending grants")

// grantStore reads and updates the shared document. Every update drops
// expired grants first, so the document holds live grants only.
type grantStore struct {
	store core.SecretStore
	ref   core.SecretRef
	clock func() time.Time
	// grace keeps a resolved device grant readable for one poll past expiry.
	grace time.Duration
}

func (g *grantStore) read(ctx context.Context) (grantState, error) {
	raw, err := core.ReadSecret(ctx, g.store, g.ref)
	if err != nil {
		return grantState{}, fmt.Errorf("read oauth state: %w", err)
	}
	return decodeGrantState(raw)
}

// update applies fn to the swept document and writes it back; an error
// from fn writes nothing. Conflicts rerun fn on the fresh document.
func (g *grantStore) update(ctx context.Context, fn func(*grantState) error) error {
	return g.store.Mutate(ctx, g.ref, func(current []byte) ([]byte, error) {
		state, err := decodeGrantState(current)
		if err != nil {
			return nil, err
		}
		state.sweep(g.clock(), g.grace)
		if err := fn(&state); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return nil, fmt.Errorf("encode oauth state: %w", err)
		}
		if len(encoded) > maxGrantStateBytes {
			return nil, errGrantStoreFull
		}
		return encoded, nil
	})
}

func (st *grantState) sweep(now time.Time, grace time.Duration) {
	for key := range st.Codes {
		if now.After(st.Codes[key].ExpiresAt) {
			delete(st.Codes, key)
		}
	}
	for key := range st.Devices {
		if now.After(st.Devices[key].ExpiresAt.Add(grace)) {
			delete(st.Devices, key)
		}
	}
}

func decodeGrantState(raw []byte) (grantState, error) {
	var state grantState
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &state); err != nil {
			return grantState{}, fmt.Errorf("decode oauth state: %w", err)
		}
	}
	if state.Codes == nil {
		state.Codes = map[string]codeGrant{}
	}
	if state.Devices == nil {
		state.Devices = map[string]deviceGrant{}
	}
	return state, nil
}
