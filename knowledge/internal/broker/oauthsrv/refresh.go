package oauthsrv

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/blob"
)

// Refresh tokens are <user>.<login>.<secret>: the login's record lives at
// refresh/<user>/<login>, so one user's logins list under one prefix, and
// each refresh replaces the secret.
const (
	// refreshSecretBytes is 256 bits per secret; only its sha256 is stored.
	refreshSecretBytes = 32
	loginNonceBytes    = 16
	// rotationGrace honors a secret rotated out this recently, minting a
	// sibling, for concurrent refreshes and a retried lost response.
	rotationGrace = time.Minute
	// maxSiblings bounds the secrets one rotation may leave valid.
	maxSiblings       = 4
	maxRotateAttempts = 4
	refreshPrefix     = "refresh/"
)

// errRefreshClientAuth refuses a login bound to a web client that did not
// authenticate as that client.
var errRefreshClientAuth = errors.New("refresh client auth failed")

// ErrRefreshTokenInvalid is every "mints nothing" case: unknown, tampered,
// expired, reused or revoked, one sentinel so a client cannot tell which.
var ErrRefreshTokenInvalid = errors.New("broker: refresh token invalid or expired")

var refreshTokenRE = regexp.MustCompile(`^([0-9a-f]{32})\.([0-9a-z]{1,13}-[0-9a-f]{32})\.([0-9a-f]{64})$`)

// refreshTokenRecord is one login: the identity it refreshes and the hashes
// of the secrets that may refresh it, never a secret.
type refreshTokenRecord struct {
	Claims    core.Claims `json:"claims"`
	IssuedAt  time.Time   `json:"issuedAt"`
	ExpiresAt time.Time   `json:"expiresAt"`
	// ClientID binds the login to the confidential web client it was
	// issued to, which must authenticate to refresh; empty is unbound.
	ClientID string `json:"clientID,omitempty"`
	// Current is the newest secret's hash, plus siblings minted in the grace.
	Current []string `json:"current"`
	// Previous was rotated out at RotatedAt; honored for rotationGrace.
	Previous  string    `json:"previous,omitempty"`
	RotatedAt time.Time `json:"rotatedAt,omitzero"`
}

// refreshToken is a parsed refresh token.
type refreshToken struct {
	user, login, secret string
}

func parseRefreshToken(raw string) (refreshToken, bool) {
	match := refreshTokenRE.FindStringSubmatch(raw)
	if match == nil {
		return refreshToken{}, false
	}
	return refreshToken{user: match[1], login: match[2], secret: match[3]}, true
}

func (t refreshToken) name() string   { return t.user + "/" + t.login }
func (t refreshToken) encode() string { return t.user + "." + t.login + "." + t.secret }

// refreshUser keys a user's logins: a hash of the IdP subject, else of the
// email, so the bucket never names the identity.
func refreshUser(claims *core.Claims) (string, error) {
	identity := "sub:" + claims.Subject
	if claims.Subject == "" {
		if claims.Email == "" {
			return "", errors.New("refresh token needs a subject or an email")
		}
		identity = "email:" + strings.ToLower(claims.Email)
	}
	h := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(h[:16]), nil
}

// loginExpiry reads the expiry a login id starts with, from a name or key
// ending in /<login>.
func loginExpiry(name string) (time.Time, bool) {
	stamp, _, dashed := strings.Cut(name[strings.LastIndex(name, "/")+1:], "-")
	seconds, err := strconv.ParseInt(stamp, 36, 64)
	if !dashed || err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// RefreshStore is the refresh token lifecycle over the state bucket: one
// object per login, rewritten by each refresh, deleted on revoke, on reuse,
// past the per-user cap or by the sweep after expiry.
type RefreshStore struct {
	cfg    *core.Config
	dir    stateDir
	log    *slog.Logger
	clock  func() time.Time
	randFn func([]byte) (int, error)
}

// NewRefreshStore wires the store with time.Now and crypto/rand; tests
// override both on the returned struct.
func NewRefreshStore(cfg *core.Config, objects blob.Store, log *slog.Logger) *RefreshStore {
	return &RefreshStore{cfg: cfg, dir: stateDir{objects: objects, prefix: refreshPrefix}, log: log, clock: time.Now, randFn: rand.Read}
}

func (s *RefreshStore) randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := s.randFn(buf); err != nil {
		return "", fmt.Errorf("generate refresh token entropy: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Issue starts a login for claims, bound to clientID when set, and returns
// its first token; the user's oldest logins past the cap are revoked.
func (s *RefreshStore) Issue(ctx context.Context, claims *core.Claims, clientID string) (string, error) {
	user, err := refreshUser(claims)
	if err != nil {
		return "", err
	}
	nonce, err := s.randomHex(loginNonceBytes)
	if err != nil {
		return "", err
	}
	secret, err := s.randomHex(refreshSecretBytes)
	if err != nil {
		return "", err
	}
	// Whole seconds, so the expiry in the login id is the record's.
	now := s.clock()
	expires := now.Add(s.cfg.Server.RefreshTokenTTL).Truncate(time.Second)
	token := refreshToken{user: user, login: strconv.FormatInt(expires.Unix(), 36) + "-" + nonce, secret: secret}
	err = s.dir.create(ctx, token.name(), refreshTokenRecord{
		Claims: *claims, IssuedAt: now, ExpiresAt: expires, ClientID: clientID, Current: []string{hashToken(secret)},
	})
	if errors.Is(err, errRecordExists) {
		return "", fmt.Errorf("refresh login id collision on %s", token.login)
	}
	if err != nil {
		return "", err
	}
	s.trimLogins(ctx, user, refreshPrefix+token.name())
	return token.encode(), nil
}

// trimLogins revokes user's soonest-expiring logins past the cap, never the
// one at keep. A failure is logged: the login it follows has succeeded.
func (s *RefreshStore) trimLogins(ctx context.Context, user, keep string) {
	logins, err := s.dir.list(ctx, user+"/")
	if err != nil {
		s.log.WarnContext(ctx, "broker: list logins for the per-user cap", "err", err)
		return
	}
	excess := len(logins) - s.cfg.Server.MaxSessionsPerUser
	if excess <= 0 {
		return
	}
	slices.SortFunc(logins, func(a, b blob.Attributes) int {
		at, _ := loginExpiry(a.Key)
		bt, _ := loginExpiry(b.Key)
		return cmp.Or(at.Compare(bt), strings.Compare(a.Key, b.Key))
	})
	revoked := 0
	for _, login := range logins {
		if revoked == excess {
			break
		}
		if login.Key == keep {
			continue
		}
		// A login refreshed since the listing is in use; the next one goes.
		done, err := s.dir.removeAt(ctx, login.Key, login.Generation)
		if err != nil {
			s.log.WarnContext(ctx, "broker: revoke a login past the per-user cap", "err", err)
			return
		}
		if done {
			revoked++
		}
	}
	s.log.InfoContext(ctx, "broker: revoked oldest logins past the per-user cap", "count", revoked, "cap", s.cfg.Server.MaxSessionsPerUser)
}

// Refresh vets rawToken's login with admit and returns the token to hand
// back: rotated for a public client, the same one for a login bound to the
// web client that authenticated as client. Any other secret is reuse.
func (s *RefreshStore) Refresh(ctx context.Context, rawToken, client string, admit func(*refreshTokenRecord) error) (string, refreshTokenRecord, error) {
	token, ok := parseRefreshToken(rawToken)
	if !ok {
		return "", refreshTokenRecord{}, ErrRefreshTokenInvalid
	}
	presented := hashToken(token.secret)
	for range maxRotateAttempts {
		var record refreshTokenRecord
		generation, err := s.dir.get(ctx, token.name(), &record)
		if errors.Is(err, blob.ErrNotFound) {
			return "", refreshTokenRecord{}, ErrRefreshTokenInvalid
		}
		if err != nil {
			return "", refreshTokenRecord{}, fmt.Errorf("read refresh login: %w", err)
		}
		now := s.clock()
		if now.After(record.ExpiresAt) {
			return "", refreshTokenRecord{}, ErrRefreshTokenInvalid
		}
		sibling := false
		switch {
		case slices.Contains(record.Current, presented):
		case presented == record.Previous && now.Sub(record.RotatedAt) <= rotationGrace:
			if len(record.Current) >= maxSiblings {
				return "", refreshTokenRecord{}, ErrRefreshTokenInvalid
			}
			sibling = true
		default:
			s.revokeReused(ctx, token, &record, generation)
			return "", refreshTokenRecord{}, ErrRefreshTokenInvalid
		}
		if record.ClientID != "" && record.ClientID != client {
			return "", record, errRefreshClientAuth
		}
		if err := admit(&record); err != nil {
			return "", record, err
		}
		if record.ClientID != "" {
			return rawToken, record, nil
		}
		secret, err := s.randomHex(refreshSecretBytes)
		if err != nil {
			return "", refreshTokenRecord{}, err
		}
		next := record
		if sibling {
			next.Current = append(slices.Clone(record.Current), hashToken(secret))
		} else {
			next.Current, next.Previous, next.RotatedAt = []string{hashToken(secret)}, presented, now
		}
		err = s.dir.replace(ctx, token.name(), generation, next)
		if errors.Is(err, errRecordChanged) {
			continue
		}
		if err != nil {
			return "", refreshTokenRecord{}, err
		}
		token.secret = secret
		return token.encode(), next, nil
	}
	return "", refreshTokenRecord{}, fmt.Errorf("refresh login %s changed on every attempt", token.login)
}

// revokeReused deletes a login whose rotated-out secret came back: a copy of
// the token is in other hands. A failed delete is logged loudly.
func (s *RefreshStore) revokeReused(ctx context.Context, token refreshToken, record *refreshTokenRecord, generation blob.Generation) {
	subject := core.HashSubject(record.Claims.Subject)
	done, err := s.dir.removeAt(ctx, refreshPrefix+token.name(), generation)
	if err == nil && !done {
		err = s.dir.remove(ctx, token.name())
	}
	if err != nil {
		s.log.ErrorContext(ctx, "broker: refresh token reused; revoking the login failed", "subject", subject, "err", err)
		return
	}
	s.log.WarnContext(ctx, "broker: refresh token reused; login revoked", "subject", subject)
}

// Revoke ends the login rawToken belongs to; an unknown token is not an
// error (RFC 7009 §2.2). Any of the login's tokens revokes it.
func (s *RefreshStore) Revoke(ctx context.Context, rawToken string) error {
	token, ok := parseRefreshToken(rawToken)
	if !ok {
		return nil
	}
	return s.dir.remove(ctx, token.name())
}

// Sweep deletes expired logins from the listing alone: a login id starts
// with its expiry, and an expired login is never rewritten.
func (s *RefreshStore) Sweep(ctx context.Context) (int, error) {
	now := s.clock()
	return s.dir.sweep(ctx, func(attrs blob.Attributes) (bool, blob.Generation, error) {
		expires, ok := loginExpiry(attrs.Key)
		return ok && now.After(expires), attrs.Generation, nil
	})
}

// hashToken is the sha256 hex of a raw token or code: the key every stored
// grant and refresh record goes under, so no raw value is stored.
func hashToken(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}
