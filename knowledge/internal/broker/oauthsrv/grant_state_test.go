package oauthsrv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	testExpiresIn    = 10 * time.Minute
	testPollInterval = 5 * time.Second
	testVerifier     = "test-verifier-must-be-43-to-128-chars-long-1234"
)

var testGrantRef = core.SecretRef{Namespace: "broker-ns", Name: "oauth-state", Key: core.OAuthStateSecretKey}

// grantFixture is one Secret store with a fake clock; replica builds the
// stores one broker replica holds over it, so a test can cross replicas.
type grantFixture struct {
	store core.SecretStore
	clock *brokertest.FakeClock
}

// lockedStore serializes Mutate, the atomic read-modify-write a real API
// server gives through resourceVersion; the fake clientset does not check it.
type lockedStore struct {
	mu sync.Mutex
	core.SecretStore
}

func (l *lockedStore) Mutate(ctx context.Context, ref core.SecretRef, mutate func([]byte) ([]byte, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.SecretStore.Mutate(ctx, ref, mutate)
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	return &grantFixture{
		store: &lockedStore{SecretStore: storage.NewK8sSecretStore(fake.NewSimpleClientset())},
		clock: brokertest.NewFakeClock(time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)),
	}
}

func (f *grantFixture) grants() *grantStore {
	return &grantStore{store: f.store, ref: testGrantRef, clock: f.clock.Now, grace: testPollInterval}
}

func (f *grantFixture) codes() *authCodeStore {
	return &authCodeStore{grants: f.grants(), codeTTL: defaultAuthCodeTTL}
}

func (f *grantFixture) devices() *deviceStore {
	return &deviceStore{grants: f.grants(), expiresIn: testExpiresIn, pollInterval: testPollInterval}
}

// document is the raw Secret value every replica shares.
func (f *grantFixture) document(t *testing.T) string {
	t.Helper()
	raw, err := core.ReadSecret(context.Background(), f.store, testGrantRef)
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	return string(raw)
}

// s256 is a verifier's PKCE S256 challenge (RFC 7636 §4.2).
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func testAuthCodeRequest() *AuthCodeRequest {
	return &AuthCodeRequest{
		ClientID:            "client-abc",
		RedirectURI:         "http://127.0.0.1:55408/callback",
		ClientState:         "client-state-nonce",
		CodeChallenge:       s256(testVerifier),
		CodeChallengeMethod: "S256",
	}
}

func aliceClaims() *core.Claims {
	claims := brokertest.AliceClaims()
	return &claims
}

// The document keeps hashes and claims: no raw code, no device code.
func TestGrantDocumentHoldsNoRawCodes(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	code, err := f.codes().Issue(ctx, testAuthCodeRequest(), aliceClaims())
	if err != nil {
		t.Fatal(err)
	}
	deviceCode, _, _, err := f.devices().Authorize(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := f.document(t)
	for name, secret := range map[string]string{"authorization code": code, "device code": deviceCode} {
		if strings.Contains(doc, secret) {
			t.Errorf("the document holds the raw %s", name)
		}
	}
	if !strings.Contains(doc, hashToken(code)) || !strings.Contains(doc, hashToken(deviceCode)) {
		t.Errorf("the document lacks the hashed keys: %s", doc)
	}
}

// Every write drops expired grants, so the document holds live ones only.
func TestGrantDocumentSweepsOnWrite(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	if _, err := f.codes().Issue(ctx, testAuthCodeRequest(), aliceClaims()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.devices().Authorize(ctx, ""); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(testExpiresIn + testPollInterval + time.Second)
	if _, err := f.codes().Issue(ctx, testAuthCodeRequest(), aliceClaims()); err != nil {
		t.Fatal(err)
	}
	st, err := f.grants().read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Codes) != 1 || len(st.Devices) != 0 {
		t.Fatalf("after a write past expiry: %d codes, %d devices; want 1 and 0", len(st.Codes), len(st.Devices))
	}
}

// Each kind is capped, and a full kind refuses only its own grants.
func TestGrantDocumentCapsEachKind(t *testing.T) {
	f := newGrantFixture(t)
	ctx := context.Background()
	err := f.grants().update(ctx, func(st *grantState) error {
		for i := range maxDeviceGrants {
			st.Devices[fmt.Sprintf("k%04d", i)] = deviceGrant{
				UserCode: "ABCDEFGH", Status: statusPending, ExpiresAt: f.clock.Now().Add(testExpiresIn),
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.devices().Authorize(ctx, ""); !errors.Is(err, errGrantStoreFull) {
		t.Fatalf("Authorize at the cap: err = %v, want errGrantStoreFull", err)
	}
	if _, err := f.codes().Issue(ctx, testAuthCodeRequest(), aliceClaims()); err != nil {
		t.Fatalf("a full device kind refused an auth code: %v", err)
	}
}

// A document past the byte budget is refused before it is written.
func TestGrantDocumentRefusesPastTheByteBudget(t *testing.T) {
	f := newGrantFixture(t)
	big := strings.Repeat("g", maxGrantStateBytes)
	err := f.grants().update(context.Background(), func(st *grantState) error {
		st.Codes["k"] = codeGrant{ClientID: big, ExpiresAt: f.clock.Now().Add(time.Minute)}
		return nil
	})
	if !errors.Is(err, errGrantStoreFull) {
		t.Fatalf("err = %v, want errGrantStoreFull", err)
	}
	if doc := f.document(t); doc != "" {
		t.Fatalf("the refused document was written: %d bytes", len(doc))
	}
}
