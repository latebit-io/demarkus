package broker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testExpiresIn    = 10 * time.Minute
	testPollInterval = 5 * time.Second
)

func newTestDeviceStore(t *testing.T) (*deviceStore, *fakeClock) {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)}
	return newDeviceStore(c.Now, testExpiresIn, testPollInterval), c
}

func TestDeviceStoreAuthorize(t *testing.T) {
	t.Run("populates state and returns formatted user code", func(t *testing.T) {
		store, clock := newTestDeviceStore(t)

		deviceCode, userCode, expiresAt, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if deviceCode == "" {
			t.Fatal("empty device code")
		}
		if !strings.Contains(userCode, "-") {
			t.Fatalf("user code missing hyphen: %q", userCode)
		}
		if len(userCode) != userCodeLen+1 {
			t.Fatalf("user code wrong length: got %d want %d", len(userCode), userCodeLen+1)
		}
		if got, want := expiresAt, clock.Now().Add(testExpiresIn); !got.Equal(want) {
			t.Fatalf("expiresAt: got %v want %v", got, want)
		}

		// Resolve back via lookup to confirm the user_code is indexed.
		gotDevice, ok := store.LookupByUserCode(userCode)
		if !ok {
			t.Fatal("LookupByUserCode returned false")
		}
		if gotDevice != deviceCode {
			t.Fatalf("user-code index mismatch: got %q want %q", gotDevice, deviceCode)
		}
	})

	t.Run("device codes are unique across calls", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		seenDevice := make(map[string]bool)
		seenUser := make(map[string]bool)
		for range 50 {
			deviceCode, userCode, _, err := store.Authorize()
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if seenDevice[deviceCode] {
				t.Fatalf("duplicate device_code: %q", deviceCode)
			}
			if seenUser[userCode] {
				t.Fatalf("duplicate user_code: %q", userCode)
			}
			seenDevice[deviceCode] = true
			seenUser[userCode] = true
		}
	})
}

func TestDeviceStoreLookupByUserCode(t *testing.T) {
	t.Run("accepts canonical, hyphenated, and whitespace forms", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, userCode, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		canonical := strings.ReplaceAll(userCode, "-", "")

		variants := []string{
			userCode,
			canonical,
			strings.ToLower(userCode),
			" " + userCode + " ",
			canonical[:4] + " " + canonical[4:],
		}
		for _, v := range variants {
			got, ok := store.LookupByUserCode(v)
			if !ok {
				t.Fatalf("variant %q: not found", v)
			}
			if got != deviceCode {
				t.Fatalf("variant %q: mismatch", v)
			}
		}
	})

	t.Run("rejects unknown / malformed inputs", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		cases := []struct {
			name  string
			input string
		}{
			{"empty", ""},
			{"too short", "ABCD"},
			{"too long", "ABCDEFGHIJK"},
			{"alphabet violation", "11110000"},
			{"never-issued", "AAAAAAAA"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, ok := store.LookupByUserCode(tc.input); ok {
					t.Fatalf("expected miss for %q", tc.input)
				}
			})
		}
	})

	t.Run("misses after expiry without sweep", func(t *testing.T) {
		store, clock := newTestDeviceStore(t)
		_, userCode, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		clock.Advance(testExpiresIn + time.Second)
		if _, ok := store.LookupByUserCode(userCode); ok {
			t.Fatal("expected lookup to miss after TTL")
		}
	})

	t.Run("misses after Bind", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, userCode, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := store.Bind(deviceCode, &ExchangeResult{}, ""); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if _, ok := store.LookupByUserCode(userCode); ok {
			t.Fatal("expected lookup to miss after Bind")
		}
	})
}

func TestDeviceStoreBind(t *testing.T) {
	t.Run("transitions pending to complete with result", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		result := ExchangeResult{
			Claims:      Claims{Subject: "sub", Email: "alice@example.com", EmailVerified: true},
			RawIDToken:  "id-token",
			AccessToken: "access-token",
		}
		if err := store.Bind(deviceCode, &result, "refresh-raw"); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		out := store.Poll(deviceCode)
		if out.Status != statusComplete {
			t.Fatalf("status: got %v want statusComplete", out.Status)
		}
		if out.Result.RawIDToken != "id-token" {
			t.Fatalf("forwarded id_token mismatch: %q", out.Result.RawIDToken)
		}
		if out.Result.AccessToken != "access-token" {
			t.Fatalf("forwarded access_token mismatch: %q", out.Result.AccessToken)
		}
		if out.RefreshToken != "refresh-raw" {
			t.Fatalf("forwarded refresh_token = %q, want refresh-raw", out.RefreshToken)
		}
	})

	t.Run("unknown device code returns not-found", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		err := store.Bind("nonexistent", &ExchangeResult{}, "")
		if !errors.Is(err, errDeviceCodeNotFound) {
			t.Fatalf("err: got %v want errDeviceCodeNotFound", err)
		}
	})

	t.Run("rejects rebind on terminal state", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := store.Bind(deviceCode, &ExchangeResult{RawIDToken: "first"}, ""); err != nil {
			t.Fatalf("first Bind: %v", err)
		}
		err = store.Bind(deviceCode, &ExchangeResult{RawIDToken: "second"}, "")
		if !errors.Is(err, errDeviceCodeTerminal) {
			t.Fatalf("second Bind: got %v want errDeviceCodeTerminal", err)
		}
		// First result must still win.
		out := store.Poll(deviceCode)
		if out.Result.RawIDToken != "first" {
			t.Fatalf("first result was overwritten: %q", out.Result.RawIDToken)
		}
	})

	t.Run("expired pending is rejected at Bind", func(t *testing.T) {
		store, clock := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		clock.Advance(testExpiresIn + time.Second)
		err = store.Bind(deviceCode, &ExchangeResult{}, "")
		if !errors.Is(err, errDeviceCodeTerminal) {
			t.Fatalf("err: got %v want errDeviceCodeTerminal", err)
		}
	})
}

func TestDeviceStoreDeny(t *testing.T) {
	t.Run("transitions pending to denied", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := store.Deny(deviceCode); err != nil {
			t.Fatalf("Deny: %v", err)
		}
		out := store.Poll(deviceCode)
		if out.Status != statusDenied {
			t.Fatalf("status: got %v want statusDenied", out.Status)
		}
	})

	t.Run("Deny after Bind is rejected", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if err := store.Bind(deviceCode, &ExchangeResult{RawIDToken: "id"}, ""); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		err = store.Deny(deviceCode)
		if !errors.Is(err, errDeviceCodeTerminal) {
			t.Fatalf("Deny: got %v want errDeviceCodeTerminal", err)
		}
	})
}

func TestDeviceStorePoll(t *testing.T) {
	t.Run("pending then complete", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if out := store.Poll(deviceCode); out.Status != statusPending || out.SlowDown {
			t.Fatalf("first poll: got %+v want pending", out)
		}
		if err := store.Bind(deviceCode, &ExchangeResult{Claims: Claims{Email: "a@b"}}, ""); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		out := store.Poll(deviceCode)
		if out.Status != statusComplete {
			t.Fatalf("post-Bind poll: got %v want complete", out.Status)
		}
		if out.Result.Claims.Email != "a@b" {
			t.Fatalf("claims forwarding broken: %+v", out.Result.Claims)
		}
	})

	t.Run("slow_down enforces minimum interval", func(t *testing.T) {
		store, clock := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if out := store.Poll(deviceCode); out.SlowDown {
			t.Fatal("first poll should not slow_down")
		}
		// Immediate second poll well under the interval.
		clock.Advance(testPollInterval / 2)
		out := store.Poll(deviceCode)
		if !out.SlowDown {
			t.Fatalf("expected slow_down, got %+v", out)
		}
		if out.Status != statusPending {
			t.Fatalf("slow_down poll must keep state pending, got %v", out.Status)
		}
		// After enough time, polling is allowed again.
		clock.Advance(testPollInterval)
		if out := store.Poll(deviceCode); out.SlowDown {
			t.Fatalf("post-interval poll should not slow_down, got %+v", out)
		}
	})

	t.Run("expiry transitions on poll", func(t *testing.T) {
		store, clock := newTestDeviceStore(t)
		deviceCode, _, _, err := store.Authorize()
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		clock.Advance(testExpiresIn + time.Second)
		out := store.Poll(deviceCode)
		if out.Status != statusExpired {
			t.Fatalf("got %v want expired", out.Status)
		}
		// Subsequent poll stays expired (idempotent terminal state).
		if out := store.Poll(deviceCode); out.Status != statusExpired {
			t.Fatalf("re-poll: got %v want expired", out.Status)
		}
	})

	t.Run("unknown device code reads as expired", func(t *testing.T) {
		store, _ := newTestDeviceStore(t)
		out := store.Poll("never-issued")
		if out.Status != statusExpired {
			t.Fatalf("got %v want expired", out.Status)
		}
	})
}

func TestDeviceStoreSweep(t *testing.T) {
	store, clock := newTestDeviceStore(t)
	deviceCode, userCode, _, err := store.Authorize()
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	canonical := strings.ReplaceAll(userCode, "-", "")

	// Before expiry: sweep is a no-op.
	store.Sweep()
	if _, ok := store.LookupByDeviceCode(deviceCode); !ok {
		t.Fatal("entry evicted before expiry")
	}
	if _, ok := store.userIndex[canonical]; !ok {
		t.Fatal("user_code index evicted before expiry")
	}

	// Past expiry + grace: entry should be gone, and the user_code freed.
	clock.Advance(testExpiresIn + testPollInterval + time.Second)
	store.Sweep()
	if _, ok := store.LookupByDeviceCode(deviceCode); ok {
		t.Fatal("entry survived post-grace sweep")
	}
	if _, ok := store.userIndex[canonical]; ok {
		t.Fatal("user_code index survived post-grace sweep")
	}
}

func TestCanonicalizeUserCode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"canonical passthrough", "WDJBMJHT", "WDJBMJHT"},
		{"with hyphen", "WDJB-MJHT", "WDJBMJHT"},
		{"lowercased", "wdjbmjht", "WDJBMJHT"},
		{"surrounding whitespace", "  WDJBMJHT  ", "WDJBMJHT"},
		{"mid whitespace", "WDJB MJHT", "WDJBMJHT"},
		{"empty", "", ""},
		{"too short", "WDJB", ""},
		{"too long", "WDJBMJHTZ", ""},
		{"alphabet violation (zero)", "WDJB0JHT", ""},
		{"alphabet violation (one)", "WDJB1JHT", ""},
		{"alphabet violation (I)", "WDJBIJHT", ""},
		{"alphabet violation (L)", "WDJBLJHT", ""},
		{"alphabet violation (O)", "WDJBOJHT", ""},
		{"alphabet violation (U)", "WDJBUJHT", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := canonicalizeUserCode(tt.in); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestUserCodeAlphabet(t *testing.T) {
	if len(userCodeAlphabet) != 30 {
		t.Fatalf("alphabet size changed: got %d want 30", len(userCodeAlphabet))
	}
	for _, forbidden := range []byte{'0', '1', 'I', 'L', 'O', 'U'} {
		if strings.IndexByte(userCodeAlphabet, forbidden) != -1 {
			t.Fatalf("forbidden char %q present in alphabet", forbidden)
		}
	}
}
