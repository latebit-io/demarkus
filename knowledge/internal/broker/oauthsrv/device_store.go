package oauthsrv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// userCodeAlphabet leaves out the confusable 0, 1, I, L, O and U; eight
// characters give 30^8 codes, and the generator still retries collisions.
const userCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// userCodeLen is the total character count (not counting the hyphen).
const userCodeLen = 8

// userCodeHyphenAt places the display hyphen; comparison strips hyphens and
// spaces and uppercases, so every typed form resolves the same.
const userCodeHyphenAt = 4

// deviceCodeBytes is the device_code's raw entropy, 64 hex characters; only
// machines handle it.
const deviceCodeBytes = 32

// deviceCodeGenAttempts caps user code collision retries; a fourth would
// only hide a broken RNG.
const deviceCodeGenAttempts = 3

// deviceStatus maps onto RFC 8628 §3.5: authorization_pending, the tokens,
// expired_token and access_denied.
type deviceStatus int

const (
	statusPending deviceStatus = iota
	statusComplete
	statusExpired
	statusDenied
)

// pollResult is what /device/token answers: slow_down while pending and
// polled early, or the terminal status, with the claims on completion.
type pollResult struct {
	Status   deviceStatus
	SlowDown bool
	Claims   core.Claims
}

// Device grant refusals. Unknown codes answer as expired at the HTTP layer,
// so the polling endpoint never says whether a code existed.
var (
	errDeviceCodeNotFound = errors.New("device code not found")
	errDeviceCodeTerminal = errors.New("device code in terminal state")
)

// deviceStore runs RFC 8628 grants in the shared grant document. A grant is
// keyed by its device_code's hash, which also rides the browser's cookies.
type deviceStore struct {
	grants       *grantStore
	expiresIn    time.Duration
	pollInterval time.Duration
}

// Authorize creates a pending grant and returns the codes the client gets.
// A user code collision with a live grant retries a few times, then fails.
func (s *deviceStore) Authorize(ctx context.Context, resource string) (deviceCode, userCode string, expiresAt time.Time, err error) {
	deviceCode, err = newDeviceCode()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("device code: %w", err)
	}
	var canonical string
	err = s.grants.update(ctx, func(st *grantState) error {
		if len(st.Devices) >= maxDeviceGrants {
			return errGrantStoreFull
		}
		var genErr error
		if canonical, genErr = freeUserCode(st); genErr != nil {
			return genErr
		}
		expiresAt = s.grants.clock().Add(s.expiresIn)
		st.Devices[hashToken(deviceCode)] = deviceGrant{UserCode: canonical, Status: statusPending, Resource: resource, ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		return "", "", time.Time{}, err
	}
	return deviceCode, formatUserCode(canonical), expiresAt, nil
}

// freeUserCode draws a user code no grant in st holds.
func freeUserCode(st *grantState) (string, error) {
	for range deviceCodeGenAttempts {
		canonical, err := newUserCode()
		if err != nil {
			return "", fmt.Errorf("user code: %w", err)
		}
		if _, taken := st.deviceByUserCode(canonical); !taken {
			return canonical, nil
		}
	}
	return "", fmt.Errorf("user code: exhausted %d collision retries", deviceCodeGenAttempts)
}

// LookupByUserCode resolves a typed user code to its pending grant's key;
// a resolved or expired grant is not found, so a flow completes once.
func (s *deviceStore) LookupByUserCode(ctx context.Context, userCode string) (key string, ok bool, err error) {
	canonical := canonicalizeUserCode(userCode)
	if canonical == "" {
		return "", false, nil
	}
	st, err := s.grants.read(ctx)
	if err != nil {
		return "", false, err
	}
	key, ok = st.deviceByUserCode(canonical)
	if !ok {
		return "", false, nil
	}
	grant := st.Devices[key]
	if grant.Status != statusPending || s.grants.clock().After(grant.ExpiresAt) {
		return "", false, nil
	}
	return key, true, nil
}

// deviceByUserCode finds the grant holding a canonical user code; codes
// are unique among live grants.
func (st *grantState) deviceByUserCode(canonical string) (string, bool) {
	for key := range st.Devices {
		if st.Devices[key].UserCode == canonical {
			return key, true
		}
	}
	return "", false
}

// Exists reports whether a grant with this key is still held.
func (s *deviceStore) Exists(ctx context.Context, key string) (bool, error) {
	st, err := s.grants.read(ctx)
	if err != nil {
		return false, err
	}
	_, ok := st.Devices[key]
	return ok, nil
}

// Bind completes a pending grant with the signed-in identity, bound to the
// grant's resource. A grant already resolved or expired is terminal.
func (s *deviceStore) Bind(ctx context.Context, key string, claims *core.Claims) error {
	return s.resolve(ctx, key, func(grant *deviceGrant) {
		grant.Status = statusComplete
		grant.Claims = claims.BoundTo(grant.Resource)
	})
}

// Deny resolves a pending grant as denied; the poller sees access_denied.
func (s *deviceStore) Deny(ctx context.Context, key string) error {
	return s.resolve(ctx, key, func(grant *deviceGrant) { grant.Status = statusDenied })
}

func (s *deviceStore) resolve(ctx context.Context, key string, apply func(*deviceGrant)) error {
	return s.grants.update(ctx, func(st *grantState) error {
		grant, ok := st.Devices[key]
		if !ok {
			return errDeviceCodeNotFound
		}
		if grant.Status != statusPending || s.grants.clock().After(grant.ExpiresAt) {
			return errDeviceCodeTerminal
		}
		apply(&grant)
		st.Devices[key] = grant
		return nil
	})
}

// Poll answers one /device/token call: a completed grant's claims, left in
// place until Collect, or its status. A pending poll is recorded for
// slow_down on every replica.
func (s *deviceStore) Poll(ctx context.Context, deviceCode string) (pollResult, error) {
	var out pollResult
	err := s.grants.update(ctx, func(st *grantState) error {
		key := hashToken(deviceCode)
		grant, ok := st.Devices[key]
		now := s.grants.clock()
		switch {
		case !ok, grant.Status == statusPending && now.After(grant.ExpiresAt):
			out = pollResult{Status: statusExpired}
		case grant.Status == statusComplete:
			out = pollResult{Status: statusComplete, Claims: grant.Claims}
		case grant.Status != statusPending:
			out = pollResult{Status: grant.Status}
		case !grant.LastPolledAt.IsZero() && now.Sub(grant.LastPolledAt) < s.pollInterval:
			out = pollResult{Status: statusPending, SlowDown: true}
		default:
			out = pollResult{Status: statusPending}
			grant.LastPolledAt = now
			st.Devices[key] = grant
		}
		return nil
	})
	return out, err
}

// Collect deletes a completed grant once its tokens are minted, so no
// claims stay behind; a grant already collected is errDeviceCodeNotFound.
func (s *deviceStore) Collect(ctx context.Context, deviceCode string) error {
	return s.grants.update(ctx, func(st *grantState) error {
		key := hashToken(deviceCode)
		if grant, ok := st.Devices[key]; !ok || grant.Status != statusComplete {
			return errDeviceCodeNotFound
		}
		delete(st.Devices, key)
		return nil
	})
}

// newDeviceCode returns a fresh opaque device_code from crypto/rand.
func newDeviceCode() (string, error) {
	buf := make([]byte, deviceCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// newUserCode draws a canonical user code uniformly from userCodeAlphabet;
// rejection sampling keeps the 30-character alphabet unbiased.
func newUserCode() (string, error) {
	const maxByte = 256 - (256 % len(userCodeAlphabet))
	out := make([]byte, 0, userCodeLen)
	buf := make([]byte, 1)
	for len(out) < userCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if int(buf[0]) >= maxByte {
			continue
		}
		out = append(out, userCodeAlphabet[int(buf[0])%len(userCodeAlphabet)])
	}
	return string(out), nil
}

// canonicalizeUserCode strips spaces and hyphens and uppercases; any
// character outside the alphabet gives "", so the lookup just misses.
func canonicalizeUserCode(in string) string {
	in = strings.ToUpper(strings.TrimSpace(in))
	out := make([]byte, 0, userCodeLen)
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '-' || c == ' ' {
			continue
		}
		if !strings.ContainsRune(userCodeAlphabet, rune(c)) {
			return ""
		}
		out = append(out, c)
		if len(out) > userCodeLen {
			return ""
		}
	}
	if len(out) != userCodeLen {
		return ""
	}
	return string(out)
}

// formatUserCode inserts the display hyphen so the user sees
// "WDJB-MJHT" rather than "WDJBMJHT". Canonical form is stored
// server-side; the hyphen is purely a readability affordance.
func formatUserCode(canonical string) string {
	if len(canonical) != userCodeLen {
		return canonical
	}
	return canonical[:userCodeHyphenAt] + "-" + canonical[userCodeHyphenAt:]
}
