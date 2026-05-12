package broker

import (
	"context"
	"errors"
	"strings"
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

	"github.com/latebit/demarkus/protocol"
)

const (
	testBrokerNS    = "broker-ns"
	testIssuancesNS = "broker-issuances"
)

func testConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:            ":0",
			CookieKey:       "dGVzdC1rZXktMTIzNDU2Nzg5MGFi",
			BrokerNamespace: testBrokerNS,
			IssuancesSecret: testIssuancesNS,
			StateTTL:        5 * time.Minute,
		},
		OIDC: OIDCConfig{
			Issuer: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "r",
		},
		Worlds: []WorldConfig{
			{
				Name:         "team-a",
				Namespace:    "team-a",
				TokensSecret: "team-a-tokens",
				AllowDomains: []string{"example.com"},
				DefaultToken: TokenScope{
					Paths:        []string{"/team-a/*"},
					Operations:   []string{"read", "publish"},
					ExpiresAfter: 24 * time.Hour,
				},
			},
		},
	}
}

func newIssuer(t *testing.T, cfg *Config, k8s *fake.Clientset) *Issuer {
	t.Helper()
	i := NewIssuer(cfg, k8s)
	i.clock = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }
	return i
}

func TestMintCreatesBothSecrets(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if got.World != "team-a" {
		t.Errorf("world = %q", got.World)
	}
	if !strings.HasPrefix(got.Label, LabelPrefix) {
		t.Errorf("label %q missing prefix", got.Label)
	}
	if got.RawToken == "" {
		t.Errorf("RawToken empty")
	}
	wantExpiry := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	if !got.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, wantExpiry)
	}

	tokensSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("tokens secret missing: %v", err)
	}
	tomlBytes := tokensSecret.Data[TokensSecretKey]
	if len(tomlBytes) == 0 {
		t.Fatalf("tokens.toml key missing or empty")
	}
	if !strings.Contains(string(tomlBytes), "[tokens."+got.Label+"]") {
		t.Errorf("tokens.toml missing label section:\n%s", tomlBytes)
	}

	issuancesSecret, err := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("issuances secret missing: %v", err)
	}
	if !strings.Contains(string(issuancesSecret.Data[IssuancesSecretKey]), `"email":"alice@example.com"`) {
		t.Errorf("issuance email not recorded:\n%s", issuancesSecret.Data[IssuancesSecretKey])
	}
}

func TestMintAppendsToExistingTokensSecret(t *testing.T) {
	existingTOML := []byte("[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: existingTOML},
	})
	i := newIssuer(t, testConfig(), k8s)

	if _, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("Mint: %v", err)
	}

	secret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	got := string(secret.Data[TokensSecretKey])
	if !strings.HasPrefix(got, string(existingTOML)) {
		t.Errorf("existing entries not preserved:\n%s", got)
	}
	if !strings.Contains(got, "[tokens.admin]") {
		t.Errorf("admin entry lost:\n%s", got)
	}
}

func TestMintRejectsUnverifiedEmail(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	// Matching domain — would otherwise be authorized — but the IdP did
	// not assert email_verified=true. Mint must refuse before reaching
	// authorizedWorlds; otherwise an attacker controlling an unverified
	// alias at an allowlisted domain could mint tokens.
	_, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: false})
	if !errors.Is(err, ErrEmailUnverified) {
		t.Errorf("err = %v, want ErrEmailUnverified", err)
	}
	if list, _ := k8s.CoreV1().Secrets("team-a").List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("expected no Secrets written, got %d", len(list.Items))
	}
}

func TestMintRejectsUnauthorizedDomain(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	_, err := i.Mint(context.Background(), Claims{Email: "mallory@evil.example", EmailVerified: true})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized", err)
	}

	if list, _ := k8s.CoreV1().Secrets("team-a").List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("expected no Secrets written, got %d", len(list.Items))
	}
}

func TestMintMultipleWorldsOnlyAuthorizedReceive(t *testing.T) {
	cfg := testConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		AllowDomains: []string{"other.example"},
		DefaultToken: TokenScope{
			Paths:        []string{"/team-b/*"},
			Operations:   []string{"read"},
			ExpiresAfter: 12 * time.Hour,
		},
	})
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, cfg, k8s)

	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(results) != 1 || results[0].World != "team-a" {
		t.Fatalf("results = %+v, want only team-a", results)
	}
	if _, err := k8s.CoreV1().Secrets("team-b").Get(context.Background(), "team-b-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("team-b Secret unexpectedly created: %v", err)
	}
}

func TestMintDomainAllowlistEmptyMatchesAll(t *testing.T) {
	cfg := testConfig()
	cfg.Worlds[0].AllowDomains = nil
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, cfg, k8s)

	if _, err := i.Mint(context.Background(), Claims{Email: "anyone@anywhere.test", EmailVerified: true}); err != nil {
		t.Errorf("Mint: %v", err)
	}
}

func TestMintLabelCollisionRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)
	var calls int32
	i.labelGen = func() (string, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return "usr_colliding", nil
		}
		return "usr_unique", nil
	}
	// Pre-populate the tokens secret with a label "usr_colliding" so the
	// first attempt hits ErrLabelExists and the issuer retries.
	existing := []byte("[tokens.usr_colliding]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	_, err := k8s.CoreV1().Secrets("team-a").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: existing},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if results[0].Label != "usr_unique" {
		t.Errorf("label = %q, want usr_unique", results[0].Label)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("labelGen called %d times, want 2", atomic.LoadInt32(&calls))
	}
}

func TestMintCrossWorldLabelCollisionRetries(t *testing.T) {
	// Two worlds, one allowed domain each. Alice qualifies for both.
	// The label generator returns "usr_dup" once, then "usr_unique"
	// thereafter. World A succeeds. World B's appendToWorldSecret does
	// NOT collide (different Secret), but appendIssuance refuses the
	// duplicate label — the issuances Secret enforces global uniqueness.
	// World B must roll back its world-Secret write and retry with the
	// next label.
	cfg := testConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		AllowDomains: []string{"example.com"},
		DefaultToken: TokenScope{
			Paths:        []string{"/team-b/*"},
			Operations:   []string{"read"},
			ExpiresAfter: 12 * time.Hour,
		},
	})
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, cfg, k8s)
	var calls int32
	i.labelGen = func() (string, error) {
		n := atomic.AddInt32(&calls, 1)
		// Calls: 1 = team-a (succeeds, lands "usr_dup")
		//        2 = team-b first attempt (also "usr_dup", collides in issuances)
		//        3 = team-b retry (returns "usr_unique")
		if n <= 2 {
			return "usr_dup", nil
		}
		return "usr_unique", nil
	}

	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Label != "usr_dup" {
		t.Errorf("team-a label = %q, want usr_dup", results[0].Label)
	}
	if results[1].Label != "usr_unique" {
		t.Errorf("team-b label = %q, want usr_unique (collision must retry)", results[1].Label)
	}

	// team-b's tokens.toml must only contain the retried label — the
	// rollback of the colliding write must have removed "usr_dup".
	secretB, _ := k8s.CoreV1().Secrets("team-b").Get(context.Background(), "team-b-tokens", metav1.GetOptions{})
	tomlB := string(secretB.Data[TokensSecretKey])
	if !strings.Contains(tomlB, "[tokens.usr_unique]") {
		t.Errorf("team-b missing retried label:\n%s", tomlB)
	}
	if strings.Contains(tomlB, "[tokens.usr_dup]") {
		t.Errorf("team-b rollback failed — collided label still present:\n%s", tomlB)
	}
}

func TestMintConflictRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	var conflicts int32
	k8s.PrependReactor("update", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.LoadInt32(&conflicts) < 2 {
			atomic.AddInt32(&conflicts, 1)
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "team-a-tokens", errors.New("simulated"))
		}
		return false, nil, nil
	})
	// Seed the world secret so the first call hits Update (not Create).
	_, err := k8s.CoreV1().Secrets("team-a").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	i := newIssuer(t, testConfig(), k8s)
	if _, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if atomic.LoadInt32(&conflicts) != 2 {
		t.Errorf("simulated conflicts = %d, want 2", atomic.LoadInt32(&conflicts))
	}
}

func TestListFiltersByEmail(t *testing.T) {
	cfg := testConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		AllowDomains: []string{"example.com"},
		DefaultToken: TokenScope{
			Paths:        []string{"/team-b/*"},
			Operations:   []string{"read"},
			ExpiresAfter: 12 * time.Hour,
		},
	})
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, cfg, k8s)

	if _, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("Mint alice: %v", err)
	}
	if _, err := i.Mint(context.Background(), Claims{Email: "bob@example.com", EmailVerified: true}); err != nil {
		t.Fatalf("Mint bob: %v", err)
	}

	got, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d issuances, want 2", len(got))
	}
	for _, g := range got {
		if g.Email != "alice@example.com" {
			t.Errorf("List returned other email: %+v", g)
		}
	}
}

func TestRevokeRemovesFromBothSecrets(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)
	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	label := results[0].Label

	if err := i.Revoke(context.Background(), "alice@example.com", label); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	secret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if strings.Contains(string(secret.Data[TokensSecretKey]), label) {
		t.Errorf("label still in tokens secret:\n%s", secret.Data[TokensSecretKey])
	}
	iss, _ := i.List(context.Background(), "alice@example.com")
	if len(iss) != 0 {
		t.Errorf("issuance still present: %+v", iss)
	}
}

func TestRevokeWrongOwner(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)
	results, _ := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})

	err := i.Revoke(context.Background(), "mallory@example.com", results[0].Label)
	if !errors.Is(err, ErrNotOwner) {
		t.Errorf("err = %v, want ErrNotOwner", err)
	}
}

func TestRevokeUnknownLabel(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	err := i.Revoke(context.Background(), "alice@example.com", "usr_nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestTokenStoredHashMatchesProtocolContract(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)
	results, _ := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	raw := results[0].RawToken

	// The server reads the hash from tokens.toml and compares it against
	// protocol.HashToken(presented). Validate the hash on disk matches
	// what the server would compute from the raw token returned to the
	// caller — the cross-binary contract Slice A centralized.
	secret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if !strings.Contains(string(secret.Data[TokensSecretKey]), `hash = "`+protocol.HashToken(raw)+`"`) {
		t.Errorf("stored hash does not match HashToken(raw):\n%s", secret.Data[TokensSecretKey])
	}
}
