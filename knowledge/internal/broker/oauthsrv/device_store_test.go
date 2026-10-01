package oauthsrv

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDeviceStoreAuthorize(t *testing.T) {
	ctx := context.Background()
	devices := newGrantFixture(t).devices()
	deviceCode, userCode, expiresAt, err := devices.Authorize(ctx, "https://mcp.example.com/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(deviceCode) != deviceCodeBytes*2 || len(userCode) != userCodeLen+1 || userCode[userCodeHyphenAt] != '-' {
		t.Errorf("device code %q, user code %q: want hex and XXXX-XXXX", deviceCode, userCode)
	}
	if expiresAt.IsZero() {
		t.Error("no expiry")
	}
	other, _, _, err := devices.Authorize(ctx, "")
	if err != nil || other == deviceCode {
		t.Fatalf("second grant: %q, %v; want a distinct code", other, err)
	}
}

func TestDeviceLookupByUserCode(t *testing.T) {
	ctx := context.Background()

	t.Run("accepts canonical, hyphenated and spaced forms", func(t *testing.T) {
		devices := newGrantFixture(t).devices()
		deviceCode, userCode, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		canonical := strings.ReplaceAll(userCode, "-", "")
		for _, typed := range []string{userCode, canonical, strings.ToLower(userCode), " " + canonical[:4] + " " + canonical[4:] + " "} {
			key, ok, err := devices.LookupByUserCode(ctx, typed)
			if err != nil || !ok || key != hashToken(deviceCode) {
				t.Errorf("%q: key %q, ok %v, err %v", typed, key, ok, err)
			}
		}
	})

	t.Run("misses unknown, expired and resolved grants", func(t *testing.T) {
		f := newGrantFixture(t)
		devices := f.devices()
		deviceCode, userCode, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, typed := range []string{"", "ZZZZ-ZZZZ", "WDJB0JHT"} {
			if _, ok, _ := devices.LookupByUserCode(ctx, typed); ok {
				t.Errorf("%q resolved", typed)
			}
		}
		if err := devices.Bind(ctx, hashToken(deviceCode), aliceClaims()); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := devices.LookupByUserCode(ctx, userCode); ok {
			t.Error("a completed grant resolved")
		}
		_, expiring, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(testExpiresIn + time.Second)
		if _, ok, _ := devices.LookupByUserCode(ctx, expiring); ok {
			t.Error("an expired grant resolved")
		}
	})
}

func TestDeviceBindAndDeny(t *testing.T) {
	ctx := context.Background()

	t.Run("resolve once; the second resolution is terminal", func(t *testing.T) {
		devices := newGrantFixture(t).devices()
		deviceCode, _, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		key := hashToken(deviceCode)
		if err := devices.Bind(ctx, key, aliceClaims()); err != nil {
			t.Fatal(err)
		}
		if err := devices.Bind(ctx, key, aliceClaims()); !errors.Is(err, errDeviceCodeTerminal) {
			t.Errorf("rebind: %v, want terminal", err)
		}
		if err := devices.Deny(ctx, key); !errors.Is(err, errDeviceCodeTerminal) {
			t.Errorf("deny after bind: %v, want terminal", err)
		}
	})

	t.Run("unknown and expired grants refuse", func(t *testing.T) {
		f := newGrantFixture(t)
		devices := f.devices()
		if err := devices.Bind(ctx, "missing", aliceClaims()); !errors.Is(err, errDeviceCodeNotFound) {
			t.Errorf("unknown: %v", err)
		}
		deviceCode, _, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(testExpiresIn + time.Second)
		if err := devices.Bind(ctx, hashToken(deviceCode), aliceClaims()); !errors.Is(err, errDeviceCodeTerminal) {
			t.Errorf("expired: %v, want terminal", err)
		}
	})
}

func TestDevicePoll(t *testing.T) {
	ctx := context.Background()

	t.Run("pending, slow_down, then the claims once", func(t *testing.T) {
		f := newGrantFixture(t)
		devices := f.devices()
		deviceCode, _, _, err := devices.Authorize(ctx, "https://mcp.example.com/mcp")
		if err != nil {
			t.Fatal(err)
		}
		poll := func() pollResult {
			t.Helper()
			out, err := devices.Poll(ctx, deviceCode)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		if out := poll(); out.Status != statusPending || out.SlowDown {
			t.Fatalf("first poll: %+v", out)
		}
		if out := poll(); !out.SlowDown {
			t.Fatalf("early poll: %+v, want slow_down", out)
		}
		f.clock.Advance(testPollInterval)
		if err := devices.Bind(ctx, hashToken(deviceCode), aliceClaims()); err != nil {
			t.Fatal(err)
		}
		out := poll()
		if out.Status != statusComplete || out.Claims.Email != "alice@example.com" || out.Claims.Resource != "https://mcp.example.com/mcp" {
			t.Fatalf("completed poll: %+v", out)
		}
		if err := devices.Collect(ctx, deviceCode); err != nil {
			t.Fatalf("Collect: %v", err)
		}
		if err := devices.Collect(ctx, deviceCode); !errors.Is(err, errDeviceCodeNotFound) {
			t.Fatalf("second Collect: %v, want not found", err)
		}
		if again := poll(); again.Status != statusExpired {
			t.Fatalf("poll after collection: %+v, want expired", again)
		}
		if doc := f.document(t); strings.Contains(doc, "alice@example.com") {
			t.Errorf("claims stayed in the document after collection: %s", doc)
		}
	})

	t.Run("denied, expired and unknown", func(t *testing.T) {
		f := newGrantFixture(t)
		devices := f.devices()
		denied, _, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := devices.Deny(ctx, hashToken(denied)); err != nil {
			t.Fatal(err)
		}
		if out, _ := devices.Poll(ctx, denied); out.Status != statusDenied {
			t.Errorf("denied: %+v", out)
		}
		if out, _ := devices.Poll(ctx, "unknown"); out.Status != statusExpired {
			t.Errorf("unknown: %+v", out)
		}
		expiring, _, _, err := devices.Authorize(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(testExpiresIn + time.Second)
		if out, _ := devices.Poll(ctx, expiring); out.Status != statusExpired {
			t.Errorf("expired: %+v", out)
		}
	})
}

// A device login crosses replicas: authorize on one, the form and the
// callback on a second, the poll on a third.
func TestDeviceGrantCrossesReplicas(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	deviceCode, userCode, _, err := f.devices().Authorize(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := f.devices().LookupByUserCode(ctx, userCode)
	if err != nil || !ok {
		t.Fatalf("lookup on a second replica: %v, %v", ok, err)
	}
	if err := f.devices().Bind(ctx, key, aliceClaims()); err != nil {
		t.Fatalf("bind on a second replica: %v", err)
	}
	if out, err := f.devices().Poll(ctx, deviceCode); err != nil || out.Status != statusComplete {
		t.Fatalf("poll on a third replica: %+v, %v", out, err)
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
