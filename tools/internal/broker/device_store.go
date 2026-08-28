package broker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// userCodeAlphabet is the character set for user-readable device codes.
// Excludes 0, 1, I, L, O, U to remove the visual confusables that trip
// up users typing the code at a verification URL on a phone or laptop.
// 30 chars; with two 4-character groups (8 total) the space is 30^8 ≈
// 6.6×10^11. Birthday-paradox collision under a 10-minute TTL with even
// pathological authorize rates stays in the negligible range, and the
// generator still retries on collision as cheap insurance.
const userCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

// userCodeLen is the total character count (not counting the hyphen).
const userCodeLen = 8

// userCodeHyphenAt is the index the display hyphen is inserted at when
// rendering for the user. Server-side comparison always strips hyphens
// + uppercases first, so a user typing "WDJB-MJHT", "wdjbmjht", or
// "wdjb mjht" with stray whitespace all resolve to the same canonical
// form.
const userCodeHyphenAt = 4

// deviceCodeBytes is the raw entropy in the opaque device_code returned
// to the polling client. 32 bytes = 64 hex chars; the device_code is
// only ever handled machine-to-machine, so length is not a UX concern.
const deviceCodeBytes = 32

// deviceCodeGenAttempts caps user-code collision retries. With the
// alphabet size above, three attempts is sufficient under any realistic
// load; a fourth would only paper over a malfunctioning RNG.
const deviceCodeGenAttempts = 3

// deviceStatus is the device-flow state-machine value. The four states
// map directly to RFC 8628 §3.5 outcomes the polling client sees on
// /device/token: authorization_pending → statusPending; success → the
// 200-with-tokens response built from statusComplete; expired_token →
// statusExpired; access_denied → statusDenied.
type deviceStatus int

const (
	statusPending deviceStatus = iota
	statusComplete
	statusExpired
	statusDenied
)

// deviceCodeState is one in-flight device-flow grant. Created by
// Authorize, mutated by Poll/Bind/Deny/Sweep, read by every handler.
// All fields are owned by the deviceStore's mutex; callers must not
// mutate after the value is returned outside the lock.
type deviceCodeState struct {
	DeviceCode   string
	UserCode     string // canonical (no hyphen, uppercase)
	Status       deviceStatus
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastPolledAt time.Time // zero until first /device/token poll

	// Populated by Bind on successful IdP exchange; consumed by Poll
	// to build the statusComplete response. Zero values otherwise.
	Result ExchangeResult
	// RefreshToken is the raw opaque refresh token minted at Bind time
	// by the broker's refreshStore. Stashed here so that idempotent
	// polling (a buggy client polling multiple times after completion
	// resolves) returns the SAME refresh_token rather than minting a
	// fresh one each poll. Empty until Bind sets it; cleared on Sweep
	// alongside the state itself.
	RefreshToken string
}

// pollResult is what the /device/token handler dispatches on. SlowDown
// is set only for a pending poll that arrived before the configured
// minimum interval elapsed — handler translates to the RFC 8628
// "slow_down" error response (status stays statusPending in the
// store; we just signal the rate violation to the handler).
type pollResult struct {
	Status   deviceStatus
	SlowDown bool
	Result   ExchangeResult // only set when Status == statusComplete
	// RefreshToken is the raw opaque refresh token bound to this grant.
	// Set only when Status == statusComplete and a refresh token was
	// minted at Bind time. Forwarded verbatim into the /device/token
	// success response; subsequent polls return the same value.
	RefreshToken string
}

// errDeviceCodeNotFound is returned by Bind / Poll / Deny when the
// device_code is unknown. Treated as expired/denied at the HTTP layer
// — never echoed to the client beyond the standard RFC 8628 error
// codes, since revealing whether a device_code ever existed would
// turn the polling endpoint into an enumeration oracle.
var errDeviceCodeNotFound = errors.New("device code not found")

// errDeviceCodeTerminal is returned by Bind when the state has already
// transitioned out of pending (already complete / expired / denied).
// The /auth/callback path treats it as a no-op success: the device
// flow already moved on, and overwriting would silently invalidate
// whichever resolution landed first.
var errDeviceCodeTerminal = errors.New("device code in terminal state")

// deviceStore is the in-memory state for all active device-flow grants
// on this broker replica. Single-broker invariant per plan §"Single
// broker only for now": no Redis, no Secret-backed persistence — a
// broker restart drops all in-flight flows and clients see
// expired_token on next poll. Acceptable for the ten-minute device-code
// TTL; revisit if HA broker ever lands.
type deviceStore struct {
	mu sync.Mutex

	codes     map[string]*deviceCodeState // device_code → state
	userIndex map[string]string           // canonical user_code → device_code

	clock        func() time.Time
	expiresIn    time.Duration
	pollInterval time.Duration
}

// newDeviceStore builds a fresh in-memory store. expiresIn is the
// device-code TTL (10m default — see plan open question §5);
// pollInterval is the minimum allowed gap between /device/token polls
// (5s default, matches the value emitted in the authorize response's
// `interval` field). clock is injected so tests can drive expiry and
// slow_down without sleeping.
func newDeviceStore(clock func() time.Time, expiresIn, pollInterval time.Duration) *deviceStore {
	if clock == nil {
		clock = time.Now
	}
	return &deviceStore{
		codes:        make(map[string]*deviceCodeState),
		userIndex:    make(map[string]string),
		clock:        clock,
		expiresIn:    expiresIn,
		pollInterval: pollInterval,
	}
}

// Authorize creates a new pending grant and returns the codes the
// /device/authorize handler echoes back to the polling client. The
// caller composes the verification_uri + verification_uri_complete
// from the broker's PublicURL; this method owns only the secret-
// material side.
//
// User-code collisions are theoretically possible (30^8 ≈ 6.6×10^11
// addresses, ~600 simultaneously-active codes under any realistic
// load); we retry up to deviceCodeGenAttempts times and surface an
// error on the unlikely third collision rather than allow a silent
// overwrite of an in-flight code.
func (s *deviceStore) Authorize() (deviceCode, userCode string, expiresAt time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	expiresAt = now.Add(s.expiresIn)

	deviceCode, err = newDeviceCode()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("device code: %w", err)
	}

	var canonical string
	for range deviceCodeGenAttempts {
		canonical, err = newUserCode()
		if err != nil {
			return "", "", time.Time{}, fmt.Errorf("user code: %w", err)
		}
		if _, taken := s.userIndex[canonical]; !taken {
			break
		}
		canonical = ""
	}
	if canonical == "" {
		return "", "", time.Time{}, fmt.Errorf("user code: exhausted %d collision retries", deviceCodeGenAttempts)
	}

	s.codes[deviceCode] = &deviceCodeState{
		DeviceCode: deviceCode,
		UserCode:   canonical,
		Status:     statusPending,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	}
	s.userIndex[canonical] = deviceCode
	return deviceCode, formatUserCode(canonical), expiresAt, nil
}

// LookupByUserCode resolves a user-typed code to its device_code. The
// /device POST handler normalizes (strip hyphens + uppercase) before
// calling; this method does the same defensively so a misuse from a
// future handler doesn't silently fail to match. Returns false on
// unknown codes and on terminal-state matches (expired / complete /
// denied) — the form handler should not let a user complete a flow
// that has already resolved.
func (s *deviceStore) LookupByUserCode(userCode string) (string, bool) {
	canonical := canonicalizeUserCode(userCode)
	if canonical == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deviceCode, ok := s.userIndex[canonical]
	if !ok {
		return "", false
	}
	state, ok := s.codes[deviceCode]
	if !ok {
		return "", false
	}
	if state.Status != statusPending {
		return "", false
	}
	if s.clock().After(state.ExpiresAt) {
		return "", false
	}
	return deviceCode, true
}

// LookupByDeviceCode returns a copy of the state for the given
// device_code, or false if absent. Used by /auth/callback's device
// branch to validate the cookie before kicking the OAuth exchange.
// Returns a copy (not a pointer) so callers can't accidentally mutate
// the store's state outside the mutex.
func (s *deviceStore) LookupByDeviceCode(deviceCode string) (deviceCodeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.codes[deviceCode]
	if !ok {
		return deviceCodeState{}, false
	}
	return *state, true
}

// Bind transitions a pending grant to statusComplete and stashes the
// IdP exchange result + the broker-minted refresh token for the
// polling client to pick up. Called from /auth/callback's device-cookie
// branch after the OAuth exchange succeeds. Takes a pointer to
// ExchangeResult to avoid copying ~120 bytes through the call site;
// the store still owns its copy via the dereference into
// deviceCodeState.Result.
//
// refreshToken may be empty during early callers; the caller is
// expected to mint and pass it. An empty value is stored verbatim and
// surfaced as an empty refresh_token in the polling response. PR4's
// deviceCallback always mints before Bind, so production paths never
// pass empty.
//
// Returns errDeviceCodeNotFound for unknown codes and
// errDeviceCodeTerminal if the grant has already resolved (already
// complete, expired, or denied) — the callback handler treats the
// terminal case as a no-op, since whatever already resolved is the
// authoritative outcome.
func (s *deviceStore) Bind(deviceCode string, result *ExchangeResult, refreshToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.codes[deviceCode]
	if !ok {
		return errDeviceCodeNotFound
	}
	now := s.clock()
	if state.Status == statusPending && now.After(state.ExpiresAt) {
		state.Status = statusExpired
	}
	if state.Status != statusPending {
		return errDeviceCodeTerminal
	}
	state.Status = statusComplete
	state.Result = *result
	state.RefreshToken = refreshToken
	return nil
}

// Deny transitions a pending grant to statusDenied. Called from
// /auth/callback's device-cookie branch when the IdP redirects back
// with an `error` query param (user denied consent at the IdP). The
// polling client sees access_denied on the next /device/token poll.
// Same terminal-state semantics as Bind.
func (s *deviceStore) Deny(deviceCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.codes[deviceCode]
	if !ok {
		return errDeviceCodeNotFound
	}
	now := s.clock()
	if state.Status == statusPending && now.After(state.ExpiresAt) {
		state.Status = statusExpired
	}
	if state.Status != statusPending {
		return errDeviceCodeTerminal
	}
	state.Status = statusDenied
	return nil
}

// Poll advances the state machine on each /device/token call. The
// returned pollResult tells the handler exactly what to write:
//   - Status==statusPending, SlowDown==false → "authorization_pending"
//   - Status==statusPending, SlowDown==true  → "slow_down"
//   - Status==statusComplete                 → success body w/ tokens
//   - Status==statusExpired                  → "expired_token"
//   - Status==statusDenied                   → "access_denied"
//
// Expiry is checked lazily on poll: a state that crossed ExpiresAt
// while still pending gets transitioned to statusExpired here, so the
// next poll sees expired_token even if the janitor hasn't swept yet.
func (s *deviceStore) Poll(deviceCode string) pollResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.codes[deviceCode]
	if !ok {
		return pollResult{Status: statusExpired}
	}
	now := s.clock()
	if state.Status == statusPending && now.After(state.ExpiresAt) {
		state.Status = statusExpired
	}
	if state.Status != statusPending {
		out := pollResult{Status: state.Status}
		if state.Status == statusComplete {
			out.Result = state.Result
			out.RefreshToken = state.RefreshToken
		}
		return out
	}
	// Plan §Open Question 4: RFC 8628 §3.5 says the server MAY require
	// the client to increase its interval by 5s on each slow_down. PR3
	// just enforces the configured minimum; if the client polls early,
	// signal slow_down without bumping. Revisit if IdP abuse becomes a
	// real signal in production.
	if !state.LastPolledAt.IsZero() && now.Sub(state.LastPolledAt) < s.pollInterval {
		return pollResult{Status: statusPending, SlowDown: true}
	}
	state.LastPolledAt = now
	return pollResult{Status: statusPending}
}

// Sweep evicts terminal entries that have aged out. Called by the
// janitor goroutine at half the device-code TTL. Pending entries past
// ExpiresAt are not eagerly transitioned here — Poll does that
// lazily on next poll — but their user_code reservation is released
// so the alphabet doesn't slowly clog up with expired-but-never-
// polled codes.
//
// retainGrace gives recently-resolved entries a short window so a
// polling client racing the sweep can still read its statusComplete
// result before the entry vanishes. One poll-interval is plenty;
// any client that hasn't polled within that window after completion
// is effectively gone.
func (s *deviceStore) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	retainGrace := s.pollInterval
	for code, state := range s.codes {
		expired := now.After(state.ExpiresAt)
		if state.Status == statusPending && expired {
			state.Status = statusExpired
		}
		terminal := state.Status != statusPending
		past := now.After(state.ExpiresAt.Add(retainGrace))
		if terminal && past {
			delete(s.codes, code)
			delete(s.userIndex, state.UserCode)
		}
	}
}

// newDeviceCode returns a fresh opaque device_code from crypto/rand.
func newDeviceCode() (string, error) {
	buf := make([]byte, deviceCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// newUserCode returns a canonical (no-hyphen, uppercase) user_code
// drawn uniformly from userCodeAlphabet. The handler formats with a
// hyphen before rendering; Lookup normalizes user input back to
// canonical form before comparing.
//
// Uses crypto/rand with rejection sampling so the alphabet's
// non-power-of-two size doesn't bias toward early characters. The
// rejection rate is well under 1% for our 30-char alphabet
// (256 % 30 = 16; reject 16/256 ≈ 6.25% per byte), so the average
// cost is one rand.Read of a tiny buffer.
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

// canonicalizeUserCode strips whitespace + hyphens and uppercases the
// input so the lookup compares against the canonical (alphabet-only)
// form stored in userIndex. Returns "" on any character outside the
// alphabet so the lookup naturally fails without exposing the
// rejection reason to the caller.
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
