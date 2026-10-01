package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/blob"
)

var refreshEpoch = time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)

func admitAll(*refreshTokenRecord) error { return nil }

// unknownRefreshToken is well formed and names no login.
func unknownRefreshToken() string {
	return refreshToken{user: strings.Repeat("0", 32), login: "0-" + strings.Repeat("0", 32), secret: strings.Repeat("0", 64)}.encode()
}

// testRefreshStore pins the clock at now, which tests move.
type testRefreshStore struct {
	*RefreshStore
	state *testState
	now   time.Time
}

func newRefreshStoreForTest(t *testing.T) *testRefreshStore {
	t.Helper()
	cfg := brokertest.NewConfig()
	cfg.Server.RefreshTokenTTL = 24 * time.Hour
	state := newTestState(t)
	s := &testRefreshStore{RefreshStore: NewRefreshStore(cfg, state, slog.Default()), state: state, now: refreshEpoch}
	s.clock = func() time.Time { return s.now }
	return s
}

func (s *testRefreshStore) issue(t *testing.T, email string, ttl time.Duration) string {
	t.Helper()
	s.cfg.Server.RefreshTokenTTL = ttl
	raw, err := s.Issue(context.Background(), &core.Claims{Subject: "sub|" + email, Email: email, EmailVerified: true}, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return raw
}

func (s *testRefreshStore) rotate(t *testing.T, raw string) string {
	t.Helper()
	next, _, err := s.Refresh(context.Background(), raw, "", admitAll)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return next
}

func (s *testRefreshStore) wantInvalid(t *testing.T, raw, why string) {
	t.Helper()
	if _, _, err := s.Refresh(context.Background(), raw, "", admitAll); !errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("%s: Refresh err = %v, want ErrRefreshTokenInvalid", why, err)
	}
}

func TestRefreshStoreIssueAndRefresh(t *testing.T) {
	s := newRefreshStoreForTest(t)
	claims := core.Claims{Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}}
	raw, err := s.Issue(context.Background(), &claims, "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	token, ok := parseRefreshToken(raw)
	if !ok {
		t.Fatalf("token %q is not <user>.<login>.<secret>", raw)
	}
	next, record, err := s.Refresh(context.Background(), raw, "", admitAll)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	rotated, ok := parseRefreshToken(next)
	if !ok || rotated.name() != token.name() || rotated.secret == token.secret {
		t.Fatalf("rotated %q, want a new secret for login %s", next, token.name())
	}
	if record.Claims.Subject != "user-1" || len(record.Claims.Groups) != 1 || record.ClientID != "" {
		t.Errorf("record = %+v", record)
	}
	if !record.ExpiresAt.Equal(record.IssuedAt.Add(24 * time.Hour)) {
		t.Errorf("ExpiresAt = %v, want IssuedAt + 24h", record.ExpiresAt)
	}
	s.rotate(t, next)
}

func TestRefreshStoreIssueNeedsAnIdentity(t *testing.T) {
	s := newRefreshStoreForTest(t)
	if _, err := s.Issue(context.Background(), &core.Claims{}, ""); err == nil {
		t.Fatal("Issue with no subject or email succeeded")
	}
}

func TestRefreshStoreRefreshRefusals(t *testing.T) {
	tests := []struct {
		name string
		fn   func(s *testRefreshStore, raw string) string
	}{
		{"unknown login", func(*testRefreshStore, string) string { return unknownRefreshToken() }},
		{"malformed", func(_ *testRefreshStore, raw string) string { return raw + "x" }},
		{"empty", func(*testRefreshStore, string) string { return "" }},
		{"expired", func(s *testRefreshStore, raw string) string {
			s.now = s.now.Add(25 * time.Hour)
			return raw
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newRefreshStoreForTest(t)
			raw := s.issue(t, "a@b.com", time.Hour)
			s.wantInvalid(t, tt.fn(s, raw), tt.name)
		})
	}
}

// A secret the login no longer honors is a copy in other hands: the login
// is revoked, so the holder of the newest token must log in again too.
func TestRefreshStoreReuseRevokesTheLogin(t *testing.T) {
	t.Run("after the grace", func(t *testing.T) {
		s := newRefreshStoreForTest(t)
		first := s.issue(t, "a@b.com", time.Hour)
		second := s.rotate(t, first)
		s.now = s.now.Add(rotationGrace + time.Second)
		s.wantInvalid(t, first, "a rotated-out secret after the grace")
		s.wantInvalid(t, second, "the newest token after reuse")
	})
	t.Run("two rotations back", func(t *testing.T) {
		s := newRefreshStoreForTest(t)
		first := s.issue(t, "a@b.com", time.Hour)
		third := s.rotate(t, s.rotate(t, first))
		s.wantInvalid(t, first, "a secret two rotations back")
		s.wantInvalid(t, third, "the newest token after reuse")
	})
	t.Run("tampered secret", func(t *testing.T) {
		s := newRefreshStoreForTest(t)
		raw := s.issue(t, "a@b.com", time.Hour)
		token, _ := parseRefreshToken(raw)
		token.secret = strings.Repeat("f", 64)
		s.wantInvalid(t, token.encode(), "a forged secret")
		s.wantInvalid(t, raw, "the real token after a forged one")
	})
}

// Within the grace the rotated-out secret mints a sibling, for concurrent
// refreshes and retried responses; the client keeps whichever it stored.
func TestRefreshStoreGraceMintsSiblings(t *testing.T) {
	s := newRefreshStoreForTest(t)
	first := s.issue(t, "a@b.com", time.Hour)
	second := s.rotate(t, first)
	s.now = s.now.Add(rotationGrace / 2)
	sibling := s.rotate(t, first)
	if sibling == second {
		t.Fatal("the sibling repeats the rotated token")
	}
	for range maxSiblings - 2 {
		s.rotate(t, first)
	}
	s.wantInvalid(t, first, "a sibling past maxSiblings")
	next := s.rotate(t, sibling)
	s.wantInvalid(t, second, "an unused sibling once another rotated")
	s.wantInvalid(t, next, "the newest token after reuse")
}

// A login bound to a web client refreshes only as that client and keeps its
// token: nothing is written, and a wrong secret is still reuse.
func TestRefreshStoreBoundLoginKeepsItsToken(t *testing.T) {
	s := newRefreshStoreForTest(t)
	raw, err := s.Issue(context.Background(), &core.Claims{Subject: "u", Email: "a@b.com"}, "web-client")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	token, _ := parseRefreshToken(raw)
	before, err := s.state.Head(context.Background(), refreshPrefix+token.name())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, _, err := s.Refresh(context.Background(), raw, "", admitAll); !errors.Is(err, errRefreshClientAuth) {
		t.Fatalf("Refresh without the bound client err = %v, want errRefreshClientAuth", err)
	}
	if _, _, err := s.Refresh(context.Background(), raw, "other-client", admitAll); !errors.Is(err, errRefreshClientAuth) {
		t.Fatalf("Refresh as another client err = %v, want errRefreshClientAuth", err)
	}
	for range 3 {
		s.now = s.now.Add(2 * rotationGrace)
		next, record, err := s.Refresh(context.Background(), raw, "web-client", admitAll)
		if err != nil || next != raw || record.ClientID != "web-client" {
			t.Fatalf("Refresh = %q, %+v, %v; want the same token", next, record, err)
		}
	}
	after, err := s.state.Head(context.Background(), refreshPrefix+token.name())
	if err != nil || after.Generation != before.Generation {
		t.Fatalf("generation %d -> %d (%v), want unchanged", before.Generation, after.Generation, err)
	}
	token.secret = strings.Repeat("f", 64)
	s.wantInvalid(t, token.encode(), "a forged secret on a bound login")
	s.wantInvalid(t, raw, "the bound token after a forged one")
}

// A refused admit writes nothing, so the token stays valid.
func TestRefreshStoreAdmitRefusalKeepsTheToken(t *testing.T) {
	s := newRefreshStoreForTest(t)
	raw := s.issue(t, "a@b.com", time.Hour)
	token, _ := parseRefreshToken(raw)
	before, err := s.state.Head(context.Background(), refreshPrefix+token.name())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	refused := errors.New("refused")
	if _, _, err := s.Refresh(context.Background(), raw, "", func(*refreshTokenRecord) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("Refresh err = %v, want the admit refusal", err)
	}
	after, err := s.state.Head(context.Background(), refreshPrefix+token.name())
	if err != nil || after.Generation != before.Generation {
		t.Fatalf("generation %d -> %d (%v), want unchanged", before.Generation, after.Generation, err)
	}
	s.rotate(t, raw)
}

// Concurrent refreshes of one token all succeed: one rotates, the rest
// retry and mint siblings.
func TestRefreshStoreConcurrentRotation(t *testing.T) {
	s := newRefreshStoreForTest(t)
	raw := s.issue(t, "a@b.com", time.Hour)
	const n = maxSiblings - 1
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			if _, _, err := s.Refresh(context.Background(), raw, "", admitAll); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// A bucket outage is a server error, never invalid_grant: a client told its
// token is invalid drops it and makes its user log in again.
func TestRefreshStoreOutageIsNotInvalid(t *testing.T) {
	s := newRefreshStoreForTest(t)
	raw := s.issue(t, "a@b.com", time.Hour)
	s.state.setErr(blob.ErrUnavailable)
	if _, _, err := s.Refresh(context.Background(), raw, "", admitAll); err == nil || errors.Is(err, ErrRefreshTokenInvalid) {
		t.Fatalf("Rotate during an outage err = %v, want a non-invalid error", err)
	}
	if err := s.Revoke(context.Background(), raw); !errors.Is(err, blob.ErrUnavailable) {
		t.Fatalf("Revoke during an outage err = %v, want ErrUnavailable", err)
	}
}

func TestRefreshStoreRevoke(t *testing.T) {
	s := newRefreshStoreForTest(t)
	first := s.issue(t, "a@b.com", time.Hour)
	second := s.rotate(t, first)
	if err := s.Revoke(context.Background(), first); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	s.wantInvalid(t, second, "any token of a revoked login")
	for _, raw := range []string{first, unknownRefreshToken(), "", "not-a-token"} {
		if err := s.Revoke(context.Background(), raw); err != nil {
			t.Errorf("Revoke(%q) = %v, want a no-op", raw, err)
		}
	}
}

// The oldest of a user's logins is revoked past the cap; other users and
// the newest logins are untouched.
func TestRefreshStorePerUserCap(t *testing.T) {
	s := newRefreshStoreForTest(t)
	s.cfg.Server.MaxSessionsPerUser = 3
	other := s.issue(t, "bob@b.com", time.Hour)
	logins := make([]string, 0, 5)
	for range 5 {
		s.now = s.now.Add(time.Second)
		logins = append(logins, s.issue(t, "a@b.com", time.Hour))
	}
	user, _ := parseRefreshToken(logins[0])
	names, err := s.dir.list(context.Background(), user.user+"/")
	if err != nil || len(names) != 3 {
		t.Fatalf("alice's logins = %v, %v; want 3", names, err)
	}
	s.wantInvalid(t, logins[0], "the oldest login past the cap")
	s.wantInvalid(t, logins[1], "the second oldest login past the cap")
	for _, raw := range append(logins[2:], other) {
		s.rotate(t, raw)
	}
}

// One object per login under refresh/<user>/<login>, holding hashes only.
func TestRefreshStoreObjectShape(t *testing.T) {
	s := newRefreshStoreForTest(t)
	raw := s.issue(t, "a@example.com", time.Hour)
	token, _ := parseRefreshToken(raw)
	listed, err := s.state.List(context.Background(), "", "")
	if err != nil || len(listed.Objects) != 1 || listed.Objects[0].Key != refreshPrefix+token.name() {
		t.Fatalf("bucket = %+v, %v; want only %s", listed.Objects, err, refreshPrefix+token.name())
	}
	object, err := s.state.Get(context.Background(), listed.Objects[0].Key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(string(object.Data), token.secret) {
		t.Fatalf("record holds the secret: %s", object.Data)
	}
	var record refreshTokenRecord
	if err := json.Unmarshal(object.Data, &record); err != nil || len(record.Current) != 1 || record.Current[0] != hashToken(token.secret) {
		t.Fatalf("record = %+v, %v; want the secret's hash as current", record, err)
	}
	if strings.Contains(token.user, "example") {
		t.Fatalf("user key %q names the identity", token.user)
	}
}

func TestRefreshStoreSweep(t *testing.T) {
	t.Run("removes expired only", func(t *testing.T) {
		s := newRefreshStoreForTest(t)
		short := s.issue(t, "short@x.com", time.Hour)
		long := s.issue(t, "long@x.com", 48*time.Hour)
		if n, err := s.Sweep(context.Background()); err != nil || n != 0 {
			t.Fatalf("Sweep before expiry = %d, %v; want 0", n, err)
		}
		s.now = s.now.Add(25 * time.Hour)
		if n, err := s.Sweep(context.Background()); err != nil || n != 1 {
			t.Fatalf("Sweep = %d, %v; want 1", n, err)
		}
		s.wantInvalid(t, short, "a swept login")
		s.rotate(t, long)
	})
	t.Run("pages past one listing", func(t *testing.T) {
		s := newRefreshStoreForTest(t)
		for i := range blob.MaxListPage + 5 {
			s.issue(t, fmt.Sprintf("u%d@x.com", i), time.Hour)
		}
		s.now = s.now.Add(25 * time.Hour)
		if n, err := s.Sweep(context.Background()); err != nil || n != blob.MaxListPage+5 {
			t.Fatalf("Sweep = %d, %v; want %d", n, err, blob.MaxListPage+5)
		}
	})
}

// Distinct logins refresh in parallel under -race.
func TestRefreshStoreConcurrentRefresh(t *testing.T) {
	s := newRefreshStoreForTest(t)
	const n = 50
	tokens := make([]string, n)
	for i := range n {
		tokens[i] = s.issue(t, fmt.Sprintf("u%d@x.com", i), time.Hour)
	}
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			_, record, err := s.Refresh(context.Background(), tokens[i], "", admitAll)
			if err != nil {
				errs <- fmt.Errorf("Refresh[%d]: %w", i, err)
				return
			}
			if want := fmt.Sprintf("u%d@x.com", i); record.Claims.Email != want {
				errs <- fmt.Errorf("Refresh[%d] email = %q, want %q", i, record.Claims.Email, want)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// Two Issues drawing the same bytes at the same instant: the second fails
// loudly instead of overwriting the first login.
func TestRefreshStoreCollision(t *testing.T) {
	s := newRefreshStoreForTest(t)
	s.randFn = func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}
	claims := &core.Claims{Email: "a@b.com"}
	if _, err := s.Issue(context.Background(), claims, ""); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if _, err := s.Issue(context.Background(), claims, ""); err == nil {
		t.Fatal("a second Issue with identical randomness succeeded")
	}
}

func TestLoginExpiry(t *testing.T) {
	stamp := time.Unix(1_800_000_000, 0)
	for _, name := range []string{"abc/", "refresh/abc/", ""} {
		key := name + strconv.FormatInt(stamp.Unix(), 36) + "-" + strings.Repeat("0", 32)
		if got, ok := loginExpiry(key); !ok || !got.Equal(stamp) {
			t.Errorf("loginExpiry(%q) = %v, %v; want %v", key, got, ok, stamp)
		}
	}
	for _, name := range []string{"", "abc", "abc/", "abc/zz", "abc/!!-00"} {
		if _, ok := loginExpiry(name); ok {
			t.Errorf("loginExpiry(%q) parsed", name)
		}
	}
}
