package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// testRefreshSecret is the operator-visible name for the broker's
// refresh-tokens Secret in tests. Distinct from testIssuancesNS so a
// stray cross-Secret read in the RefreshStore would land on a
// missing Secret and surface as an obvious test failure rather than
// silently picking up issuance bytes.
const testRefreshSecret = "broker-refresh-tokens"

// testRefreshConfig builds a Config sufficient for RefreshStore
// tests. Reuses the issuance-test config fields so a future cross-
// store interaction (Step 2 onward) shares one base config helper.
func testRefreshConfig() *Config {
	c := testConfig()
	c.Server.RefreshTokensSecret = testRefreshSecret
	c.Server.RefreshTokenTTL = 24 * time.Hour
	return c
}

func newRefreshStoreForTest(t *testing.T, k8s *fake.Clientset) *RefreshStore {
	t.Helper()
	s := NewRefreshStore(testRefreshConfig(), NewK8sSecretStore(k8s))
	s.clock = func() time.Time { return time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestRefreshStoreIssueRoundTrip(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	s := newRefreshStoreForTest(t, k8s)
	ctx := context.Background()

	claims := Claims{Subject: "user-1", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}}
	raw, err := s.Issue(ctx, &claims, "", s.cfg.Server.RefreshTokenTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(raw) != refreshTokenBytes*2 {
		t.Errorf("raw token length = %d, want %d hex chars", len(raw), refreshTokenBytes*2)
	}

	rec, err := s.Refresh(ctx, raw)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if rec.Claims.Subject != claims.Subject {
		t.Errorf("Claims.Subject = %q, want %q", rec.Claims.Subject, claims.Subject)
	}
	if rec.Claims.Email != claims.Email {
		t.Errorf("Claims.Email = %q, want %q", rec.Claims.Email, claims.Email)
	}
	if rec.Claims.EmailVerified != claims.EmailVerified {
		t.Errorf("Claims.EmailVerified = %v", rec.Claims.EmailVerified)
	}
	if len(rec.Claims.Groups) != 1 || rec.Claims.Groups[0] != "eng" {
		t.Errorf("Claims.Groups = %v", rec.Claims.Groups)
	}
	if rec.KeyHash != hashRefreshToken(raw) {
		t.Errorf("KeyHash mismatch")
	}
	if rec.IssuedAt.IsZero() {
		t.Errorf("IssuedAt zero")
	}
	if !rec.ExpiresAt.Equal(rec.IssuedAt.Add(24 * time.Hour)) {
		t.Errorf("ExpiresAt = %v, want IssuedAt + 24h", rec.ExpiresAt)
	}
	if rec.LastUsedAt.IsZero() {
		t.Errorf("LastUsedAt zero after Refresh")
	}
}

func TestRefreshStoreIssueRejectsZeroTTL(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	s := newRefreshStoreForTest(t, k8s)
	if _, err := s.Issue(context.Background(), &Claims{}, "", 0); err == nil {
		t.Fatalf("Issue with ttl=0 should error")
	}
	if _, err := s.Issue(context.Background(), &Claims{}, "", -time.Second); err == nil {
		t.Fatalf("Issue with negative ttl should error")
	}
}

func TestRefreshStoreRefreshErrors(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s *RefreshStore, raw string) (string, time.Time)
	}{
		{"unknown token", func(_ *testing.T, _ *RefreshStore, _ string) (string, time.Time) {
			// Same length as a real token, never issued.
			return strings.Repeat("0", refreshTokenBytes*2), time.Time{}
		}},
		{"tampered token (one byte off)", func(_ *testing.T, _ *RefreshStore, raw string) (string, time.Time) {
			// Flip the first hex character to a guaranteed-different value.
			flipped := []byte(raw)
			if flipped[0] == 'f' {
				flipped[0] = '0'
			} else {
				flipped[0] = 'f'
			}
			return string(flipped), time.Time{}
		}},
		{"empty token", func(_ *testing.T, _ *RefreshStore, _ string) (string, time.Time) {
			return "", time.Time{}
		}},
		{"expired token", func(_ *testing.T, s *RefreshStore, raw string) (string, time.Time) {
			// Jump clock past ExpiresAt.
			base := s.clock()
			s.clock = func() time.Time { return base.Add(25 * time.Hour) }
			return raw, base.Add(25 * time.Hour)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := fake.NewSimpleClientset()
			s := newRefreshStoreForTest(t, k8s)
			ctx := context.Background()
			raw, err := s.Issue(ctx, &Claims{Subject: "u", Email: "a@b.com", EmailVerified: true}, "", time.Hour)
			if err != nil {
				t.Fatalf("setup Issue: %v", err)
			}
			input, _ := tt.fn(t, s, raw)
			if _, err := s.Refresh(ctx, input); !errors.Is(err, ErrRefreshTokenInvalid) {
				t.Fatalf("Refresh(%q) err = %v, want ErrRefreshTokenInvalid", input, err)
			}
		})
	}
}

func TestRefreshStoreRevoke(t *testing.T) {
	t.Run("revoke valid then refresh fails", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		ctx := context.Background()
		raw, err := s.Issue(ctx, &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if err := s.Revoke(ctx, raw); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, err := s.Refresh(ctx, raw); !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Fatalf("Refresh after Revoke err = %v, want ErrRefreshTokenInvalid", err)
		}
	})
	t.Run("revoke unknown is no-op", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		// Secret may or may not exist — Revoke must not error either way.
		if err := s.Revoke(context.Background(), strings.Repeat("0", refreshTokenBytes*2)); err != nil {
			t.Fatalf("Revoke unknown (no Secret): %v", err)
		}
		// No-write contract: Revoke against an absent Secret must NOT
		// materialize the Secret as a side effect. Verifies the
		// mutateSecret skip-on-empty-result guard.
		if _, err := k8s.CoreV1().Secrets(s.cfg.Server.BrokerNamespace).Get(context.Background(), testRefreshSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Secret should not exist after no-op Revoke; Get err = %v", err)
		}
		// Now after an Issue + Revoke, revoke-unknown still succeeds.
		raw, err := s.Issue(context.Background(), &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if err := s.Revoke(context.Background(), raw); err != nil {
			t.Fatalf("Revoke real: %v", err)
		}
		if err := s.Revoke(context.Background(), raw); err != nil {
			t.Fatalf("Revoke again (idempotent): %v", err)
		}
	})
	t.Run("revoke empty is no-op", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		if err := s.Revoke(context.Background(), ""); err != nil {
			t.Fatalf("Revoke empty: %v", err)
		}
		// Same no-write contract as the unknown-token case.
		if _, err := k8s.CoreV1().Secrets(s.cfg.Server.BrokerNamespace).Get(context.Background(), testRefreshSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Secret should not exist after empty-token Revoke; Get err = %v", err)
		}
	})
}

func TestRefreshStoreSweep(t *testing.T) {
	t.Run("removes expired only", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		ctx := context.Background()
		base := s.clock()

		// Mint one short-lived (1h) and one long-lived (48h) token.
		shortRaw, err := s.Issue(ctx, &Claims{Email: "short@x.com", EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("Issue short: %v", err)
		}
		longRaw, err := s.Issue(ctx, &Claims{Email: "long@x.com", EmailVerified: true}, "", 48*time.Hour)
		if err != nil {
			t.Fatalf("Issue long: %v", err)
		}

		// Advance clock past the short one's ExpiresAt but not the long one's.
		s.clock = func() time.Time { return base.Add(2 * time.Hour) }

		n, err := s.Sweep(ctx)
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n != 1 {
			t.Errorf("Sweep returned %d, want 1", n)
		}
		// Short is gone, long survives.
		if _, err := s.Refresh(ctx, shortRaw); !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Errorf("expected expired token swept; Refresh err=%v", err)
		}
		if _, err := s.Refresh(ctx, longRaw); err != nil {
			t.Errorf("long-lived token must survive sweep: %v", err)
		}
	})
	t.Run("no-op on empty store", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		n, err := s.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n != 0 {
			t.Errorf("Sweep on empty returned %d, want 0", n)
		}
		// No-write contract: Sweep against an absent Secret must
		// NOT materialize the Secret as a side effect.
		if _, err := k8s.CoreV1().Secrets(s.cfg.Server.BrokerNamespace).Get(context.Background(), testRefreshSecret, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Secret should not exist after no-op Sweep; Get err = %v", err)
		}
	})
	t.Run("no-op when nothing expired", func(t *testing.T) {
		k8s := fake.NewSimpleClientset()
		s := newRefreshStoreForTest(t, k8s)
		_, err := s.Issue(context.Background(), &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		n, err := s.Sweep(context.Background())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n != 0 {
			t.Errorf("Sweep returned %d, want 0 (nothing expired yet)", n)
		}
	})
}

func TestRefreshStoreSecretShape(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	s := newRefreshStoreForTest(t, k8s)
	ctx := context.Background()

	raw, err := s.Issue(ctx, &Claims{Subject: "u1", Email: "a@example.com", EmailVerified: true, Groups: []string{"g"}}, "", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	sec, err := k8s.CoreV1().Secrets(s.cfg.Server.BrokerNamespace).Get(ctx, testRefreshSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get Secret: %v", err)
	}
	body := sec.Data[RefreshTokensSecretKey]
	if len(body) == 0 {
		t.Fatalf("Secret data[%s] empty", RefreshTokensSecretKey)
	}
	// Raw token MUST NEVER appear in the persisted Secret.
	if strings.Contains(string(body), raw) {
		t.Fatalf("raw refresh token leaked into Secret body")
	}

	var parsed refreshTokens
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	keyHash := hashRefreshToken(raw)
	rec, ok := parsed.Entries[keyHash]
	if !ok {
		t.Fatalf("entries missing keyHash %s", keyHash)
	}
	if rec.KeyHash != keyHash {
		t.Errorf("record.KeyHash = %q, want %q", rec.KeyHash, keyHash)
	}
	if rec.Claims.Email != "a@example.com" {
		t.Errorf("claims.email = %q", rec.Claims.Email)
	}
}

// TestRefreshStoreConcurrentRefresh issues sequentially (the fake
// clientset doesn't enforce optimistic-concurrency on Update, so
// parallel Issues without a conflict-emitting reactor would last-
// writer-wins and lose entries) then refreshes in parallel under
// -race. The race detector is the load-bearing check: any
// unsynchronized read or write inside RefreshStore surfaces here.
func TestRefreshStoreConcurrentRefresh(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	s := newRefreshStoreForTest(t, k8s)
	ctx := context.Background()

	const n = 50
	tokens := make([]string, n)
	for i := range n {
		raw, err := s.Issue(ctx, &Claims{Subject: fmt.Sprintf("u%d", i), Email: fmt.Sprintf("u%d@x.com", i), EmailVerified: true}, "", time.Hour)
		if err != nil {
			t.Fatalf("setup Issue[%d]: %v", i, err)
		}
		tokens[i] = raw
	}

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			rec, err := s.Refresh(ctx, tokens[idx])
			if err != nil {
				errs <- fmt.Errorf("Refresh[%d]: %w", idx, err)
				return
			}
			wantEmail := fmt.Sprintf("u%d@x.com", idx)
			if rec.Claims.Email != wantEmail {
				errs <- fmt.Errorf("Refresh[%d] email = %q, want %q", idx, rec.Claims.Email, wantEmail)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}
}

// TestRefreshStoreIssueConflictRetries exercises mutateSecret's
// optimistic-concurrency retry loop end-to-end through RefreshStore.
// A reactor injects two consecutive Conflicts on Update; the third
// attempt must succeed and the issued token must be retrievable.
// Mirrors TestMintConflictRetries' shape for the issuances Secret.
func TestRefreshStoreIssueConflictRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	var conflicts int32
	k8s.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		if ua.GetNamespace() != testBrokerNS {
			return false, nil, nil
		}
		if atomic.LoadInt32(&conflicts) < 2 {
			atomic.AddInt32(&conflicts, 1)
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, testRefreshSecret, errors.New("simulated"))
		}
		return false, nil, nil
	})
	// Seed the Secret so the first Issue hits Update (not Create) —
	// the conflict path lives on Update.
	seed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testRefreshSecret, Namespace: testBrokerNS},
		Data:       map[string][]byte{},
	}
	if _, err := k8s.CoreV1().Secrets(testBrokerNS).Create(context.Background(), seed, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newRefreshStoreForTest(t, k8s)
	raw, err := s.Issue(context.Background(), &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if atomic.LoadInt32(&conflicts) != 2 {
		t.Errorf("simulated conflicts = %d, want 2", atomic.LoadInt32(&conflicts))
	}
	if _, err := s.Refresh(context.Background(), raw); err != nil {
		t.Errorf("Refresh after retried Issue: %v", err)
	}
}

func TestRefreshStoreCollisionRetry(t *testing.T) {
	// Inject a randFn that yields the same bytes on the first call,
	// then real randomness afterward. The first Issue stores the
	// collision-candidate; the second must detect the duplicate hash
	// and fail loudly rather than overwrite.
	k8s := fake.NewSimpleClientset()
	s := newRefreshStoreForTest(t, k8s)
	fixed := make([]byte, refreshTokenBytes)
	for i := range fixed {
		fixed[i] = byte(i)
	}
	calls := 0
	s.randFn = func(b []byte) (int, error) {
		calls++
		copy(b, fixed)
		return len(b), nil
	}
	ctx := context.Background()
	if _, err := s.Issue(ctx, &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	if _, err := s.Issue(ctx, &Claims{Email: "a@b.com", EmailVerified: true}, "", time.Hour); err == nil {
		t.Fatalf("second Issue with identical randomness should fail with collision")
	}
	if calls != 2 {
		t.Errorf("randFn calls = %d, want 2", calls)
	}
}
