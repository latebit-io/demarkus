package broker

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPendingAuthCodeTTL = 10 * time.Minute
	testAuthCodeTTL        = 60 * time.Second
)

func newTestAuthCodeStore(t *testing.T) (*authCodeStore, *fakeClock) {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)}
	return newAuthCodeStore(c.Now, testPendingAuthCodeTTL, testAuthCodeTTL), c
}

// pkce returns a verifier/challenge pair for tests. The verifier is
// the input to /token; the challenge is what the broker stored at
// /oauth/authorize. Mirrors RFC 7636 §4.2 + §4.6.
func pkce(verifier string) (codeVerifier, codeChallenge string) {
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func newTestAuthCodeRequest() *AuthCodeRequest {
	_, challenge := pkce("test-verifier-must-be-43-to-128-chars-long-1234")
	return &AuthCodeRequest{
		ClientID:            "client-abc",
		RedirectURI:         "http://127.0.0.1:55408/callback",
		ClientState:         "client-state-nonce",
		Scope:               "openid email profile",
		CodeChallenge:       challenge,
		CodeChallengeMethod: "S256",
	}
}

func TestAuthCodeStoreBegin(t *testing.T) {
	t.Run("returns distinct ids and stores the request verbatim", func(t *testing.T) {
		store, clock := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()

		id1, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		id2, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if id1 == "" || id2 == "" {
			t.Fatal("empty id")
		}
		if id1 == id2 {
			t.Fatal("duplicate ids across Begins")
		}

		p, ok := store.LookupPending(id1)
		if !ok {
			t.Fatal("LookupPending returned false for fresh id")
		}
		if p.ClientID != req.ClientID || p.RedirectURI != req.RedirectURI ||
			p.ClientState != req.ClientState || p.Scope != req.Scope ||
			p.CodeChallenge != req.CodeChallenge ||
			p.CodeChallengeMethod != req.CodeChallengeMethod {
			t.Fatalf("pending request fields mutated: got %+v want %+v", p.AuthCodeRequest, req)
		}
		if got, want := p.ExpiresAt, clock.Now().Add(testPendingAuthCodeTTL); !got.Equal(want) {
			t.Fatalf("expiresAt: got %v want %v", got, want)
		}
	})

	t.Run("ids are unique across many Begins", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		seen := make(map[string]bool)
		for range 50 {
			id, err := store.Begin(req)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if seen[id] {
				t.Fatalf("duplicate id: %q", id)
			}
			seen[id] = true
		}
	})
}

func TestAuthCodeStoreLookupPending(t *testing.T) {
	t.Run("unknown id returns false", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		if _, ok := store.LookupPending("never-issued"); ok {
			t.Fatal("expected miss on unknown id")
		}
	})

	t.Run("expired pending returns false", func(t *testing.T) {
		store, clock := newTestAuthCodeStore(t)
		id, err := store.Begin(newTestAuthCodeRequest())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		clock.Advance(testPendingAuthCodeTTL + time.Second)
		if _, ok := store.LookupPending(id); ok {
			t.Fatal("expected miss after TTL")
		}
	})
}

func TestAuthCodeStoreBind(t *testing.T) {
	t.Run("issues a code, removes the pending entry, returns the request", func(t *testing.T) {
		store, clock := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		exchange := ExchangeResult{
			Claims:      Claims{Subject: "sub-1", Email: "alice@example.com", EmailVerified: true},
			RawIDToken:  "id-token-raw",
			AccessToken: "access-token-raw",
			Expiry:      clock.Now().Add(1 * time.Hour),
		}

		code, pending, err := store.Bind(id, &exchange)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if code == "" {
			t.Fatal("empty code")
		}
		if pending.ClientID != req.ClientID || pending.ClientState != req.ClientState ||
			pending.RedirectURI != req.RedirectURI {
			t.Fatalf("pending mirror mismatch: got %+v want %+v", pending.AuthCodeRequest, req)
		}

		if _, ok := store.LookupPending(id); ok {
			t.Fatal("pending entry survived Bind")
		}
	})

	t.Run("unknown id returns not-found", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		if _, _, err := store.Bind("nonexistent", &ExchangeResult{}); !errors.Is(err, errAuthCodePendingNotFound) {
			t.Fatalf("err: got %v want errAuthCodePendingNotFound", err)
		}
	})

	t.Run("rebind on the same id is rejected", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		id, err := store.Begin(newTestAuthCodeRequest())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, _, err := store.Bind(id, &ExchangeResult{RawIDToken: "first"}); err != nil {
			t.Fatalf("first Bind: %v", err)
		}
		_, _, err = store.Bind(id, &ExchangeResult{RawIDToken: "second"})
		if !errors.Is(err, errAuthCodePendingNotFound) {
			t.Fatalf("second Bind: got %v want errAuthCodePendingNotFound", err)
		}
	})

	t.Run("expired pending is rejected", func(t *testing.T) {
		store, clock := newTestAuthCodeStore(t)
		id, err := store.Begin(newTestAuthCodeRequest())
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		clock.Advance(testPendingAuthCodeTTL + time.Second)
		if _, _, err := store.Bind(id, &ExchangeResult{}); !errors.Is(err, errAuthCodePendingNotFound) {
			t.Fatalf("got %v want errAuthCodePendingNotFound", err)
		}
		// Pending entry is also dropped so the next sweep is a no-op.
		if _, ok := store.LookupPending(id); ok {
			t.Fatal("expired pending entry should have been removed at Bind time")
		}
	})

	t.Run("codes are unique across many Binds", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		seen := make(map[string]bool)
		for range 50 {
			id, err := store.Begin(newTestAuthCodeRequest())
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			code, _, err := store.Bind(id, &ExchangeResult{})
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if seen[code] {
				t.Fatalf("duplicate code: %q", code)
			}
			seen[code] = true
		}
	})
}

func TestAuthCodeStoreRedeem(t *testing.T) {
	verifier, _ := pkce("test-verifier-must-be-43-to-128-chars-long-1234")

	t.Run("returns the bound exchange on success and is one-shot", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		exchange := ExchangeResult{
			Claims:      Claims{Subject: "sub-1", Email: "alice@example.com", EmailVerified: true},
			RawIDToken:  "id-token-raw",
			AccessToken: "access-token-raw",
		}
		code, _, err := store.Bind(id, &exchange)
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}

		got, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier)
		if err != nil {
			t.Fatalf("Redeem: %v", err)
		}
		if got.RawIDToken != exchange.RawIDToken || got.AccessToken != exchange.AccessToken {
			t.Fatalf("exchange forwarding mismatch: got %+v want %+v", got, exchange)
		}

		// Second Redeem fails — one-shot.
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("replay: got %v want errAuthCodeNotFound", err)
		}
	})

	t.Run("unknown code returns not-found", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		if _, err := store.Redeem("never-issued", req.ClientID, req.RedirectURI, verifier); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("got %v want errAuthCodeNotFound", err)
		}
	})

	t.Run("expired code returns not-found and is removed", func(t *testing.T) {
		store, clock := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		code, _, err := store.Bind(id, &ExchangeResult{})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		clock.Advance(testAuthCodeTTL + time.Second)
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("expired Redeem: got %v want errAuthCodeNotFound", err)
		}
		// Re-Redeem after expiry-driven delete still returns not-found.
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier); !errors.Is(err, errAuthCodeNotFound) {
			t.Fatalf("post-delete Redeem: got %v want errAuthCodeNotFound", err)
		}
	})

	t.Run("transient client misconfig: client_id mismatch is rejected and preserves code", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		code, _, err := store.Bind(id, &ExchangeResult{})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if _, err := store.Redeem(code, "wrong-client", req.RedirectURI, verifier); !errors.Is(err, errAuthCodeClientMismatch) {
			t.Fatalf("got %v want errAuthCodeClientMismatch", err)
		}
		// Failed Redeem must NOT consume the entry — the legitimate
		// client could still come in. (Whether to invalidate here is
		// a tradeoff; we choose to keep the code alive so a transient
		// client misconfig doesn't burn the user's flow.)
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier); err != nil {
			t.Fatalf("legitimate Redeem after mismatch failed: %v", err)
		}
	})

	t.Run("redirect_uri mismatch is rejected", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		code, _, err := store.Bind(id, &ExchangeResult{})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if _, err := store.Redeem(code, req.ClientID, "http://127.0.0.1:99999/other", verifier); !errors.Is(err, errAuthCodeRedirectMismatch) {
			t.Fatalf("got %v want errAuthCodeRedirectMismatch", err)
		}
	})

	t.Run("pkce verifier mismatch is rejected", func(t *testing.T) {
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		code, _, err := store.Bind(id, &ExchangeResult{})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, "wrong-verifier"); !errors.Is(err, errAuthCodePKCEFailed) {
			t.Fatalf("got %v want errAuthCodePKCEFailed", err)
		}
	})

	t.Run("non-S256 stored method always fails verification", func(t *testing.T) {
		// Defense-in-depth: the /oauth/authorize handler rejects
		// method != S256 before Begin. If a future refactor lets a
		// `plain` request through, Redeem must still fail closed.
		store, _ := newTestAuthCodeStore(t)
		req := newTestAuthCodeRequest()
		req.CodeChallengeMethod = "plain"
		req.CodeChallenge = "any-plain-value"
		id, err := store.Begin(req)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		code, _, err := store.Bind(id, &ExchangeResult{})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if _, err := store.Redeem(code, req.ClientID, req.RedirectURI, "any-plain-value"); !errors.Is(err, errAuthCodePKCEFailed) {
			t.Fatalf("got %v want errAuthCodePKCEFailed", err)
		}
	})
}

func TestAuthCodeStoreSweep(t *testing.T) {
	store, clock := newTestAuthCodeStore(t)

	// Two pending grants, one issued code.
	idLive, err := store.Begin(newTestAuthCodeRequest())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	idExpired, err := store.Begin(newTestAuthCodeRequest())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	idBindable, err := store.Begin(newTestAuthCodeRequest())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	codeExpired, _, err := store.Bind(idBindable, &ExchangeResult{})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Pre-expiry sweep: no-op.
	store.Sweep()
	if _, ok := store.LookupPending(idLive); !ok {
		t.Fatal("live pending evicted prematurely")
	}
	if _, ok := store.LookupPending(idExpired); !ok {
		t.Fatal("about-to-expire pending evicted prematurely")
	}

	// Advance past the code TTL but inside the pending TTL.
	clock.Advance(testAuthCodeTTL + time.Second)
	store.Sweep()
	if _, ok := store.LookupPending(idLive); !ok {
		t.Fatal("live pending evicted by code-TTL sweep")
	}
	// Issued code must be gone.
	verifier, _ := pkce("test-verifier-must-be-43-to-128-chars-long-1234")
	if _, err := store.Redeem(codeExpired, "client-abc", "http://127.0.0.1:55408/callback", verifier); !errors.Is(err, errAuthCodeNotFound) {
		t.Fatalf("expired code survived sweep: %v", err)
	}

	// Advance past the pending TTL.
	clock.Advance(testPendingAuthCodeTTL + time.Second)
	store.Sweep()
	if _, ok := store.LookupPending(idLive); ok {
		t.Fatal("expired pending survived sweep")
	}
	if _, ok := store.LookupPending(idExpired); ok {
		t.Fatal("expired pending survived sweep")
	}
}

func TestAuthCodeStoreConcurrentRedeemOneWinner(t *testing.T) {
	// Two Redeem calls race on the same code; exactly one must win.
	store, _ := newTestAuthCodeStore(t)
	req := newTestAuthCodeRequest()
	id, err := store.Begin(req)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	code, _, err := store.Bind(id, &ExchangeResult{RawIDToken: "id-token"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	verifier, _ := pkce("test-verifier-must-be-43-to-128-chars-long-1234")

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := store.Redeem(code, req.ClientID, req.RedirectURI, verifier)
			results <- err
		})
	}
	wg.Wait()
	close(results)

	var success, notFound int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, errAuthCodeNotFound):
			notFound++
		default:
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if success != 1 || notFound != 1 {
		t.Fatalf("expected 1 success + 1 not-found, got success=%d not-found=%d", success, notFound)
	}
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "test-verifier-must-be-43-to-128-chars-long-1234"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	tests := []struct {
		name      string
		challenge string
		method    string
		verifier  string
		want      bool
	}{
		{"happy path S256", challenge, "S256", verifier, true},
		{"wrong verifier", challenge, "S256", "wrong", false},
		{"non-S256 method", challenge, "plain", verifier, false},
		{"empty method", challenge, "", verifier, false},
		{"empty challenge", "", "S256", verifier, false},
		{"empty verifier", challenge, "S256", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := verifyPKCE(tt.challenge, tt.method, tt.verifier); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestNewAuthCodeAndID(t *testing.T) {
	t.Run("auth code id is hex of expected length", func(t *testing.T) {
		id, err := newAuthCodeID()
		if err != nil {
			t.Fatalf("newAuthCodeID: %v", err)
		}
		if len(id) != authCodeIDBytes*2 {
			t.Fatalf("len: got %d want %d", len(id), authCodeIDBytes*2)
		}
	})
	t.Run("auth code is base64url no padding", func(t *testing.T) {
		code, err := newAuthCode()
		if err != nil {
			t.Fatalf("newAuthCode: %v", err)
		}
		if strings.ContainsAny(code, "=+/") {
			t.Fatalf("expected base64url-no-pad, got %q", code)
		}
	})
}
