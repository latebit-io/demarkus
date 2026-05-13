package broker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
				Allow:        AllowConfig{Domains: []string{"example.com"}},
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
	assertNoSecretsWritten(t, k8s)
}

func TestMintRejectsUnauthorizedDomain(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	_, err := i.Mint(context.Background(), Claims{Email: "mallory@evil.example", EmailVerified: true})
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized", err)
	}
	assertNoSecretsWritten(t, k8s)
}

func TestMintRejectsEmptyEmail(t *testing.T) {
	// World allowlist-empty would otherwise authorize anyone, including
	// an empty/whitespace-only email — collapsing every future caller
	// with no identity into the same "owner" in the issuances Secret.
	// Mint must refuse before that ownership record is written.
	cfg := testConfig()
	cfg.Worlds[0].Allow = AllowConfig{}
	for _, email := range []string{"", "   ", "\t"} {
		k8s := fake.NewSimpleClientset()
		i := newIssuer(t, cfg, k8s)
		_, err := i.Mint(context.Background(), Claims{Email: email, EmailVerified: true})
		if !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("email=%q: err = %v, want ErrNotAuthorized", email, err)
		}
		assertNoSecretsWritten(t, k8s)
	}
}

func TestMintCanonicalizesEmail(t *testing.T) {
	// Mint trims and lowercases claims.Email once before authorization
	// and persistence. Without that, a domain match on the right side
	// of the email could be silently dropped because LastIndex("@")
	// keeps trailing whitespace in the domain slice; and two logins of
	// the same user with different IdP-side casing would land as two
	// distinct "owner" records in the issuances Secret. Asserting the
	// final state covers both halves: domain match succeeded (single
	// MintResult returned) and the persisted owner is canonical.
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)

	results, err := i.Mint(context.Background(), Claims{
		Email:         "  Alice@Example.COM\t",
		EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List got %d, want 1", len(got))
	}
	if got[0].Email != "alice@example.com" {
		t.Errorf("issuance Email = %q, want canonical %q", got[0].Email, "alice@example.com")
	}
}

// assertNoSecretsWritten verifies the rejection paths leave both the
// world namespace and the broker namespace untouched. Bug regressions
// that write the issuances Secret before returning an error would
// otherwise slip past a world-only check.
func assertNoSecretsWritten(t *testing.T, k8s *fake.Clientset) {
	t.Helper()
	if list, _ := k8s.CoreV1().Secrets("team-a").List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Errorf("team-a Secrets unexpectedly written: %d", len(list.Items))
	}
	if _, err := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("issuances secret unexpectedly created: %v", err)
	}
}

func TestMintMultipleWorldsOnlyAuthorizedReceive(t *testing.T) {
	cfg := testConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		Allow:        AllowConfig{Domains: []string{"other.example"}},
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

func TestMintAllAllowlistsEmptyMatchesAll(t *testing.T) {
	// Back-compat: a world with no allowlist (the pre-Slice-C default
	// when an operator omitted allowDomains) keeps the "any verified
	// user qualifies" behavior. Slice C.1 must not tighten this — only
	// rendering AllowConfig non-empty switches into restricted mode.
	cfg := testConfig()
	cfg.Worlds[0].Allow = AllowConfig{}
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, cfg, k8s)

	if _, err := i.Mint(context.Background(), Claims{Email: "anyone@anywhere.test", EmailVerified: true}); err != nil {
		t.Errorf("Mint: %v", err)
	}
}

// TestMintAuthorizationPredicate is the Slice C.1 authorization table.
// Each row pins one AllowConfig shape and asserts whether a given
// Claims qualifies. Mint is the public surface; reaching ErrNotAuthorized
// means the world rejected, while a single MintResult means it accepted.
// The "expect" column is the world-A accept decision; rejections also
// verify no Secrets were written (caught by the partial-mint regression
// guard from Slice B).
func TestMintAuthorizationPredicate(t *testing.T) {
	tests := []struct {
		name   string
		allow  AllowConfig
		claims Claims
		accept bool
	}{
		{
			name:   "all_empty_back_compat_accept",
			allow:  AllowConfig{},
			claims: Claims{Email: "any@anywhere.test", EmailVerified: true},
			accept: true,
		},
		{
			name:   "domain_only_match",
			allow:  AllowConfig{Domains: []string{"example.com"}},
			claims: Claims{Email: "alice@example.com", EmailVerified: true},
			accept: true,
		},
		{
			name:   "domain_only_reject",
			allow:  AllowConfig{Domains: []string{"example.com"}},
			claims: Claims{Email: "alice@evil.example", EmailVerified: true},
			accept: false,
		},
		{
			name:   "groups_only_match",
			allow:  AllowConfig{Groups: []string{"engineering"}},
			claims: Claims{Email: "any@anywhere.test", EmailVerified: true, Groups: []string{"engineering", "ops"}},
			accept: true,
		},
		{
			name: "groups_only_reject_no_group_claim",
			// The IdP did not surface a `groups` claim at all (Groups nil).
			// AllowGroups is non-empty so groupsMatch returns false.
			// The operator's escape hatch is to add Emails — covered below.
			allow:  AllowConfig{Groups: []string{"engineering"}},
			claims: Claims{Email: "any@anywhere.test", EmailVerified: true},
			accept: false,
		},
		{
			name:   "groups_only_reject_wrong_group",
			allow:  AllowConfig{Groups: []string{"engineering"}},
			claims: Claims{Email: "any@anywhere.test", EmailVerified: true, Groups: []string{"marketing"}},
			accept: false,
		},
		{
			name: "groups_case_insensitive_claim_side",
			// The allowlist arrives pre-normalized (lowercased+trimmed)
			// from config load — see TestLoadConfig/allow.groups_normalized_to_lowercase
			// for the load-side half. This row asserts the runtime half:
			// the IdP can emit any case in the claim and still match.
			// Together the two cover the end-to-end "operator writes
			// any case, IdP emits any case" story. Regressing this to a
			// case-sensitive compare would silently break the common
			// case where Okta/Entra/Auth0 normalize group casing
			// upstream and must fail this test.
			allow:  AllowConfig{Groups: []string{"engineering"}},
			claims: Claims{Email: "any@anywhere.test", EmailVerified: true, Groups: []string{"Engineering"}},
			accept: true,
		},
		{
			name:   "domain_and_groups_both_match",
			allow:  AllowConfig{Domains: []string{"example.com"}, Groups: []string{"engineering"}},
			claims: Claims{Email: "alice@example.com", EmailVerified: true, Groups: []string{"engineering"}},
			accept: true,
		},
		{
			name: "domain_and_groups_domain_fails",
			// Predicate is AND between domains and groups, so a domain
			// miss rejects even with a matching group. This is the
			// "groups intersect domains" semantics — RBAC inside an org,
			// not across orgs.
			allow:  AllowConfig{Domains: []string{"example.com"}, Groups: []string{"engineering"}},
			claims: Claims{Email: "alice@partner.test", EmailVerified: true, Groups: []string{"engineering"}},
			accept: false,
		},
		{
			name:   "domain_and_groups_groups_fails",
			allow:  AllowConfig{Domains: []string{"example.com"}, Groups: []string{"engineering"}},
			claims: Claims{Email: "alice@example.com", EmailVerified: true, Groups: []string{"marketing"}},
			accept: false,
		},
		{
			name:   "emails_carve_out_bypasses_domain",
			allow:  AllowConfig{Domains: []string{"example.com"}, Emails: []string{"alice@partner.test"}},
			claims: Claims{Email: "alice@partner.test", EmailVerified: true},
			accept: true,
		},
		{
			name: "emails_carve_out_bypasses_groups",
			// The whole point of Emails: the user lacks a matching
			// group (or any groups at all), but is on the carve-out
			// list. Mint must accept regardless.
			allow:  AllowConfig{Groups: []string{"engineering"}, Emails: []string{"contractor@example.com"}},
			claims: Claims{Email: "contractor@example.com", EmailVerified: true},
			accept: true,
		},
		{
			name: "emails_only_no_match_rejects",
			// The "only AllowEmails" subtlety: an empty Domains+Groups
			// would normally evaluate to true on the AND predicate, but
			// worldAllows refuses to fall through to that branch when
			// only Emails was configured. Otherwise `allow.emails:
			// [alice]` with nothing else would silently mean "everyone
			// plus alice."
			allow:  AllowConfig{Emails: []string{"alice@example.com"}},
			claims: Claims{Email: "bob@example.com", EmailVerified: true},
			accept: false,
		},
		{
			name:   "emails_only_match",
			allow:  AllowConfig{Emails: []string{"alice@example.com"}},
			claims: Claims{Email: "alice@example.com", EmailVerified: true},
			accept: true,
		},
		{
			name: "emails_case_insensitive",
			// AllowConfig.Emails is normalized at config load
			// (lowercased + trimmed). The runtime compare lowercases
			// the claim's email too; a claim with uppercase characters
			// must still match a lowercased entry.
			allow:  AllowConfig{Emails: []string{"alice@example.com"}},
			claims: Claims{Email: "Alice@Example.COM", EmailVerified: true},
			accept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Worlds[0].Allow = tt.allow
			k8s := fake.NewSimpleClientset()
			i := newIssuer(t, cfg, k8s)

			results, err := i.Mint(context.Background(), tt.claims)
			if tt.accept {
				if err != nil {
					t.Fatalf("Mint: %v, want accept", err)
				}
				if len(results) != 1 {
					t.Errorf("got %d results, want 1", len(results))
				}
				return
			}
			if !errors.Is(err, ErrNotAuthorized) {
				t.Errorf("err = %v, want ErrNotAuthorized", err)
			}
			assertNoSecretsWritten(t, k8s)
		})
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

func TestMintLabelCollisionExhaustsRetries(t *testing.T) {
	// All maxLabelRetries attempts return the same already-taken label.
	// Exercises the terminal error path in mintForWorld after the retry
	// budget is exhausted — distinct from the successful-retry case in
	// TestMintLabelCollisionRetries. Counts labelGen calls to verify
	// the loop actually iterated the full budget rather than bailing
	// early.
	k8s := fake.NewSimpleClientset()
	i := newIssuer(t, testConfig(), k8s)
	var calls int32
	i.labelGen = func() (string, error) {
		atomic.AddInt32(&calls, 1)
		return "usr_forever", nil
	}
	existing := []byte("[tokens.usr_forever]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	_, err := k8s.CoreV1().Secrets("team-a").Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: existing},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err = i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err == nil || !strings.Contains(err.Error(), "consecutive label collisions") {
		t.Errorf("err = %v, want 'consecutive label collisions'", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxLabelRetries) {
		t.Errorf("labelGen called %d times, want %d (full retry budget)", got, maxLabelRetries)
	}
	// No issuance record should have been written.
	if _, err := k8s.CoreV1().Secrets(testBrokerNS).Get(context.Background(), testIssuancesNS, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("issuances secret unexpectedly created: %v", err)
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
		Allow:        AllowConfig{Domains: []string{"example.com"}},
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
		Allow:        AllowConfig{Domains: []string{"example.com"}},
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

func TestRevokeMissingWorldPreservesIssuance(t *testing.T) {
	// Seed the issuances Secret with a record pointing at a world the
	// broker is no longer configured for (operator removed it from the
	// YAML between mint and revoke). Revoke must surface the
	// misconfiguration rather than silently dropping the issuance entry
	// while leaving a still-valid token in the orphaned world's
	// tokens.toml. The issuance record must stay so an operator can
	// investigate.
	cfg := testConfig()
	orphan := Issuance{
		Label:      "usr_orphan",
		Email:      "alice@example.com",
		World:      "team-removed",
		Paths:      []string{"/team-removed/*"},
		Operations: []string{"read"},
		IssuedAt:   time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(Issuances{Entries: []Issuance{orphan}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testIssuancesNS, Namespace: testBrokerNS},
		Data:       map[string][]byte{IssuancesSecretKey: payload},
	})
	i := newIssuer(t, cfg, k8s)

	err = i.Revoke(context.Background(), "alice@example.com", "usr_orphan")
	if err == nil {
		t.Fatalf("Revoke returned nil, want error for missing world")
	}
	if !strings.Contains(err.Error(), "team-removed") {
		t.Errorf("error %q does not name the orphaned world", err)
	}

	// Issuance must still be there for the operator to find.
	got, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Label != "usr_orphan" {
		t.Errorf("issuance dropped or mutated: %+v", got)
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

func TestMintRBACDeniedNoPartialState(t *testing.T) {
	// Slice C.2 RBAC-denied guard. Multi-world config: alice qualifies
	// for team-a and team-b. The broker SA has write permission on
	// team-a's tokens Secret but is denied on team-b's. Mint must
	// return partial results (team-a) plus an error, AND leave team-b
	// with zero state in either the world Secret or the issuances
	// Secret. The latter is the "no partial state" property the plan
	// §6.2 partial-mint edge case promises: a failed world's mint
	// rolls back any half-written state before returning.
	//
	// Implementation detail: we pre-seed team-b's tokens Secret with
	// empty data so mutateSecret takes the Update path (rather than
	// Create), which matches the real-world RBAC failure mode of "SA
	// can patch but only on the listed Secrets in its Role." Deny the
	// update via a fake.Clientset reactor returning Forbidden.
	cfg := testConfig()
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "team-b",
		Namespace:    "team-b",
		TokensSecret: "team-b-tokens",
		Allow:        AllowConfig{Domains: []string{"example.com"}},
		DefaultToken: TokenScope{
			Paths:        []string{"/team-b/*"},
			Operations:   []string{"read"},
			ExpiresAfter: 12 * time.Hour,
		},
	})
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-b-tokens", Namespace: "team-b"},
		Data:       map[string][]byte{TokensSecretKey: {}},
	})
	k8s.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		if ua.GetNamespace() == "team-b" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "secrets"},
				"team-b-tokens",
				errors.New("broker SA lacks update on team-b/team-b-tokens"),
			)
		}
		return false, nil, nil
	})
	i := newIssuer(t, cfg, k8s)

	results, err := i.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err == nil {
		t.Fatal("Mint returned nil err, want RBAC-denied")
	}
	if len(results) != 1 || results[0].World != "team-a" {
		t.Fatalf("results = %+v, want only team-a (partial mint)", results)
	}
	if !strings.Contains(err.Error(), "team-b") {
		t.Errorf("err %q does not name the failing world team-b", err)
	}

	// team-b's tokens Secret was seeded empty and must STAY empty —
	// every Update was Forbidden, so neither the initial write nor any
	// retry could land. (mutateSecret's optimistic-concurrency retry
	// only re-fires on Conflict, not Forbidden, so the budget isn't
	// burned and the call returns promptly.)
	teamB, err := k8s.CoreV1().Secrets("team-b").Get(context.Background(), "team-b-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get team-b Secret: %v", err)
	}
	if len(teamB.Data[TokensSecretKey]) != 0 {
		t.Errorf("team-b tokens Secret leaked data despite RBAC denial:\n%s", teamB.Data[TokensSecretKey])
	}

	// Issuances Secret records team-a only — team-b never made it
	// past the world-Secret write, so the per-world rollback in
	// mintForWorld (which only fires after a successful world write
	// followed by a failed issuance append) didn't even need to run.
	live, err := i.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("issuances after RBAC denial = %d, want 1 (team-a)", len(live))
	}
	if live[0].World != "team-a" {
		t.Errorf("issuance world = %q, want team-a", live[0].World)
	}
}

func TestRotateLabelHappyPath(t *testing.T) {
	// End-to-end: mint via Issuer.Mint, rotate the resulting label,
	// assert the new MintResult is well-formed and the world/issuance
	// Secrets reflect exactly the new label (old gone from both).
	// This pins the basic "mint new, revoke old" sequence on the
	// happy path.
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, testConfig(), k8s)
	minted, err := issuer.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed Mint: %v", err)
	}
	oldLabel := minted[0].Label
	oldToken := minted[0].RawToken

	rotated, err := issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, oldLabel)
	if err != nil {
		t.Fatalf("RotateLabel: %v", err)
	}
	if rotated.Label == "" || rotated.Label == oldLabel {
		t.Errorf("new label = %q, want non-empty and != old %q", rotated.Label, oldLabel)
	}
	if rotated.RawToken == "" || rotated.RawToken == oldToken {
		t.Errorf("new RawToken empty or unchanged from old")
	}
	if rotated.World != "team-a" {
		t.Errorf("world = %q, want team-a", rotated.World)
	}

	// World Secret: only the new label present.
	worldSecret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get world Secret: %v", err)
	}
	gotTOML := string(worldSecret.Data[TokensSecretKey])
	if strings.Contains(gotTOML, oldLabel) {
		t.Errorf("old label still in world Secret after rotate:\n%s", gotTOML)
	}
	if !strings.Contains(gotTOML, rotated.Label) {
		t.Errorf("new label missing from world Secret:\n%s", gotTOML)
	}

	// Issuances Secret: only the new label, with same scope, same email.
	live, err := issuer.List(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("issuances after rotate = %d, want 1", len(live))
	}
	if live[0].Label != rotated.Label {
		t.Errorf("issuance label = %q, want rotated label %q", live[0].Label, rotated.Label)
	}
	if live[0].Email != "alice@example.com" {
		t.Errorf("issuance email = %q, want alice@example.com (canonical)", live[0].Email)
	}
}

func TestRotateLabelNotOwner(t *testing.T) {
	// Alice mints, mallory tries to rotate. Same gate as Revoke's
	// owner-check: ErrNotOwner, no state change in either Secret.
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, testConfig(), k8s)
	minted, err := issuer.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed Mint: %v", err)
	}
	oldLabel := minted[0].Label

	_, err = issuer.RotateLabel(context.Background(), Claims{Email: "mallory@example.com", EmailVerified: true}, oldLabel)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}

	// Alice's token must still be intact in both Secrets.
	worldSecret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if !strings.Contains(string(worldSecret.Data[TokensSecretKey]), oldLabel) {
		t.Errorf("alice's label vanished from world Secret after mallory's failed rotate")
	}
	live, _ := issuer.List(context.Background(), "alice@example.com")
	if len(live) != 1 || live[0].Label != oldLabel {
		t.Errorf("alice's issuance changed after mallory's failed rotate: %+v", live)
	}
}

func TestRotateLabelUnknownLabel(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, testConfig(), k8s)

	_, err := issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, "usr_nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestRotateLabelPreservesIssuanceScope(t *testing.T) {
	// The plan's central rotate semantic: scope (paths + operations)
	// stays frozen to the issuance record across rotation. Operator
	// narrowing of DefaultToken.Paths between mint and rotate must
	// NOT shrink the rotated token's reach — that only happens on
	// next demarkus login. Lifetime, by contrast, DOES update to the
	// operator's current DefaultToken.ExpiresAfter (separate test
	// below would cover the lifetime side; here we only need to pin
	// scope).
	cfg := testConfig()
	cfg.Worlds[0].DefaultToken.Paths = []string{"/team-a/wide/*", "/team-a/other/*"}
	cfg.Worlds[0].DefaultToken.Operations = []string{"read", "publish"}
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, cfg, k8s)
	minted, err := issuer.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed Mint: %v", err)
	}
	oldLabel := minted[0].Label

	// Operator narrows DefaultToken between mint and rotate.
	// (Mutating the shared config is fine here — newIssuer captured
	// the same *Config pointer, but the issuance record's scope was
	// snapshot at mint time and stored in the issuances Secret.)
	cfg.Worlds[0].DefaultToken.Paths = []string{"/team-a/narrow/*"}
	cfg.Worlds[0].DefaultToken.Operations = []string{"read"}

	rotated, err := issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, oldLabel)
	if err != nil {
		t.Fatalf("RotateLabel: %v", err)
	}

	// New issuance record carries the ORIGINAL scope, not the
	// narrowed DefaultToken.
	live, _ := issuer.List(context.Background(), "alice@example.com")
	if len(live) != 1 || live[0].Label != rotated.Label {
		t.Fatalf("issuances = %+v, want one entry matching rotated", live)
	}
	wantPaths := []string{"/team-a/wide/*", "/team-a/other/*"}
	if !slices.Equal(live[0].Paths, wantPaths) {
		t.Errorf("paths = %v, want %v (issuance-frozen, not config-current)", live[0].Paths, wantPaths)
	}
	wantOps := []string{"read", "publish"}
	if !slices.Equal(live[0].Operations, wantOps) {
		t.Errorf("operations = %v, want %v (issuance-frozen)", live[0].Operations, wantOps)
	}

	// World Secret entry for the new label carries the original
	// scope too — the server is the one that enforces paths/ops, so
	// what's written here is what the user can actually do.
	worldSecret, _ := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	gotTOML := string(worldSecret.Data[TokensSecretKey])
	if !strings.Contains(gotTOML, `paths = ["/team-a/wide/*", "/team-a/other/*"]`) {
		t.Errorf("world Secret entry does not carry original paths:\n%s", gotTOML)
	}
}

func TestRotateLabelLifetimeUpdatesToCurrentConfig(t *testing.T) {
	// The other half of the scope-vs-lifetime asymmetry: lifetime
	// resets to now + DefaultToken.ExpiresAfter on rotate, using the
	// operator's CURRENT config. If the operator tightens expiry,
	// rotation honors the new shorter lifetime — that's an explicit
	// security-tightening lever and rotation must not bypass it.
	cfg := testConfig()
	cfg.Worlds[0].DefaultToken.ExpiresAfter = 24 * time.Hour
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, cfg, k8s)
	minted, err := issuer.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed Mint: %v", err)
	}

	// Tighten expiry to 1 hour and rotate.
	cfg.Worlds[0].DefaultToken.ExpiresAfter = 1 * time.Hour

	rotated, err := issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, minted[0].Label)
	if err != nil {
		t.Fatalf("RotateLabel: %v", err)
	}

	// newIssuer pins the clock to 2026-05-11 12:00 UTC; expect the
	// new token to expire 1h later (the tightened lifetime), not 24h
	// later (the original).
	wantExpiry := time.Date(2026, 5, 11, 13, 0, 0, 0, time.UTC)
	if !rotated.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v (1h after pinned clock — operator tightening must apply)",
			rotated.ExpiresAt, wantExpiry)
	}
}

func TestRotateLabelReauthorizesAgainstCurrentAllow(t *testing.T) {
	// Rotate-as-relogin: if the user no longer qualifies under the
	// world's current Allow predicate (operator removed their group,
	// renamed their domain, took them off the email carve-out),
	// rotation must reject. Without re-auth, a fired user could keep
	// rotating forever as long as the IdP still issues ID tokens.
	cfg := testConfig()
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"example.com"}}
	k8s := fake.NewSimpleClientset()
	issuer := newIssuer(t, cfg, k8s)
	minted, err := issuer.Mint(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true})
	if err != nil {
		t.Fatalf("seed Mint: %v", err)
	}

	// Operator tightens the allowlist to a different domain. Alice's
	// email no longer qualifies.
	cfg.Worlds[0].Allow = AllowConfig{Domains: []string{"other.example"}}

	_, err = issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, minted[0].Label)
	if !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized", err)
	}

	// Alice's original token must remain intact — rotate failed
	// before any state mutation. (No new label minted, no old label
	// revoked.)
	live, _ := issuer.List(context.Background(), "alice@example.com")
	if len(live) != 1 || live[0].Label != minted[0].Label {
		t.Errorf("issuances after denied rotate = %+v, want unchanged from original", live)
	}
}

func TestRotateLabelMissingWorldErrors(t *testing.T) {
	// Pre-seed an issuance pointing at a world the broker has no
	// config for (admin removed the world between mint and rotate).
	// RotateLabel must surface the misconfiguration rather than
	// silently treating it as no-op or 5xx-swallowing; same
	// conservative posture as Revoke.
	cfg := testConfig()
	orphan := Issuance{
		Label:      "usr_orphan",
		Email:      "alice@example.com",
		World:      "team-removed",
		Paths:      []string{"/team-removed/*"},
		Operations: []string{"read"},
		IssuedAt:   time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(Issuances{Entries: []Issuance{orphan}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testIssuancesNS, Namespace: testBrokerNS},
		Data:       map[string][]byte{IssuancesSecretKey: payload},
	})
	issuer := newIssuer(t, cfg, k8s)

	_, err = issuer.RotateLabel(context.Background(), Claims{Email: "alice@example.com", EmailVerified: true}, "usr_orphan")
	if err == nil {
		t.Fatal("RotateLabel returned nil, want error for missing world")
	}
	if !strings.Contains(err.Error(), "team-removed") {
		t.Errorf("error %q does not name the orphaned world", err)
	}
}
