package broker

import (
	"context"
	"errors"
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

const (
	testBrokerNS = "broker-ns"
	// testIssuancesNS names the broker-namespace Secret that the
	// removed issuance subsystem used to write. The broker no longer
	// mints per-user tokens, so this Secret must never appear; several
	// tests (install, sweeper) assert its absence as a no-write guard.
	testIssuancesNS = "broker-issuances"
)

func testConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Addr:            ":0",
			CookieKey:       "dGVzdC1rZXktMTIzNDU2Nzg5MGFi",
			BrokerNamespace: testBrokerNS,
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
					Paths: []string{"/team-a/*"},
				},
			},
		},
	}
}

// TestReadableWorldsIgnoresWriterAllow pins the read/write split: reads are
// gated by the broker SSO org gate alone, so readableWorlds returns every
// configured world regardless of its writer Allow. (authorizedWorlds — the
// writer set — is tested via TestWorldAllowsPredicate.)
func TestReadableWorldsIgnoresWriterAllow(t *testing.T) {
	cfg := testConfig() // team-a: Allow domains=example.com
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "locked",
		Namespace:    "locked",
		TokensSecret: "locked-tokens",
		Allow:        AllowConfig{Emails: []string{"only-admin@nowhere.test"}},
	})
	got := readableWorlds(cfg)
	if len(got) != 2 {
		t.Fatalf("readableWorlds = %d, want 2 (a restrictive writer Allow must not hide a world from readers)", len(got))
	}
	seen := map[string]bool{}
	for _, w := range got {
		seen[w.Name] = true
	}
	if !seen["team-a"] || !seen["locked"] {
		t.Errorf("readableWorlds names = %v, want both team-a and locked", seen)
	}
}

// TestWorldAllowsPredicate is the authorization table for the
// AllowConfig predicate. Each row pins one AllowConfig shape and asserts
// whether a given Claims qualifies. Tests worldAllows directly — the
// broker no longer mints per-user tokens, so the predicate is the whole
// authorization surface (the MCP write gate and /me/install both reduce
// to this call). Coverage migrated from the former TestMintAuthorizationPredicate.
func TestWorldAllowsPredicate(t *testing.T) {
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
			// list. worldAllows must accept regardless.
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
			if got := worldAllows(&tt.allow, &tt.claims); got != tt.accept {
				t.Errorf("worldAllows = %v, want %v", got, tt.accept)
			}
		})
	}
}

// TestAuthorizedWorlds covers the multi-world reduction over the
// predicate: authorizedWorlds returns exactly the worlds whose Allow
// admits the claims, in cfg.Worlds declaration order. Migrated from the
// former TestMintMultipleWorldsOnlyAuthorizedReceive and
// TestMintAllAllowlistsEmptyMatchesAll, which routed the same matrix
// through the removed Mint path.
func TestAuthorizedWorlds(t *testing.T) {
	t.Run("only_authorized_worlds_returned", func(t *testing.T) {
		// Two worlds, distinct allowed domains. alice@example.com
		// qualifies only for team-a; team-b (other.example) must be
		// excluded from the result.
		cfg := testConfig()
		cfg.Worlds = append(cfg.Worlds, WorldConfig{
			Name:      "team-b",
			Namespace: "team-b",
			Allow:     AllowConfig{Domains: []string{"other.example"}},
		})
		got := authorizedWorlds(cfg, &Claims{Email: "alice@example.com", EmailVerified: true})
		if len(got) != 1 || got[0].Name != "team-a" {
			names := make([]string, len(got))
			for i, w := range got {
				names[i] = w.Name
			}
			t.Errorf("authorizedWorlds = %v, want [team-a]", names)
		}
	})

	t.Run("empty_allowlist_matches_any", func(t *testing.T) {
		// Back-compat: a world with no allowlist (the pre-Slice-C
		// default when an operator omitted allowDomains) keeps the
		// "any verified user qualifies" behavior. Only rendering
		// AllowConfig non-empty switches into restricted mode.
		cfg := testConfig()
		cfg.Worlds[0].Allow = AllowConfig{}
		got := authorizedWorlds(cfg, &Claims{Email: "anyone@anywhere.test", EmailVerified: true})
		if len(got) != 1 || got[0].Name != "team-a" {
			t.Errorf("authorizedWorlds = %+v, want team-a admitted on empty allowlist", got)
		}
	})

	t.Run("unauthorized_identity_gets_none", func(t *testing.T) {
		cfg := testConfig()
		got := authorizedWorlds(cfg, &Claims{Email: "mallory@evil.example", EmailVerified: true})
		if len(got) != 0 {
			t.Errorf("authorizedWorlds = %+v, want empty for unauthorized domain", got)
		}
	})

	t.Run("declaration_order_preserved", func(t *testing.T) {
		// Both worlds admit alice; the result must be in cfg.Worlds
		// declaration order so /me/install can reason about ordering.
		cfg := testConfig()
		cfg.Worlds = append(cfg.Worlds, WorldConfig{
			Name:      "team-b",
			Namespace: "team-b",
			Allow:     AllowConfig{Domains: []string{"example.com"}},
		})
		got := authorizedWorlds(cfg, &Claims{Email: "alice@example.com", EmailVerified: true})
		if len(got) != 2 || got[0].Name != "team-a" || got[1].Name != "team-b" {
			t.Errorf("authorizedWorlds order = %+v, want [team-a, team-b]", got)
		}
	})
}

// TestLookupWorld pins the name resolver shared by the MCP write gate
// and the per-world write-token store: exact-name match or nil.
func TestLookupWorld(t *testing.T) {
	cfg := testConfig()
	if w := lookupWorld(cfg, "team-a"); w == nil || w.Name != "team-a" {
		t.Errorf("lookupWorld(team-a) = %+v, want team-a", w)
	}
	if w := lookupWorld(cfg, "nope"); w != nil {
		t.Errorf("lookupWorld(nope) = %+v, want nil", w)
	}
}

// TestOIDCDomainAllowed pins the broker-global hd allowlist semantics:
// empty list opens the gate, non-empty list demands an hd match.
// Consumer Google accounts (empty hd) must be rejected when the list
// is set — this is the whole point of keying on hd vs the email
// domain, which an unverified secondary can spoof.
func TestOIDCDomainAllowed(t *testing.T) {
	tests := []struct {
		name   string
		allow  []string
		hd     string
		accept bool
	}{
		{"empty_list_open", nil, "anything.com", true},
		{"empty_list_empty_hd_open", nil, "", true},
		{"match", []string{"latebit.io"}, "latebit.io", true},
		{"match_mixed_case_hd", []string{"latebit.io"}, "Latebit.IO", true},
		{"miss_other_workspace", []string{"latebit.io"}, "competitor.com", false},
		{"miss_empty_hd_consumer_google", []string{"latebit.io"}, "", false},
		{"miss_whitespace_only", []string{"latebit.io"}, "   ", false},
		{"multi_domain_match", []string{"latebit.io", "nesto.test"}, "nesto.test", true},
		{"multi_domain_miss", []string{"latebit.io", "nesto.test"}, "outsider.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := oidcDomainAllowed(tt.allow, tt.hd)
			if got != tt.accept {
				t.Errorf("oidcDomainAllowed(%v, %q) = %v, want %v", tt.allow, tt.hd, got, tt.accept)
			}
		})
	}
}

// TestMutateSecretConflictRetries exercises mutateSecret's optimistic-
// concurrency retry loop directly: a reactor fails the first two Update
// calls with a resourceVersion Conflict, then lets the write through.
// mutateSecret must re-read and retry, converging without surfacing the
// transient conflict. Migrated from the former TestMintConflictRetries,
// which drove the same loop through the removed Mint path.
func TestMutateSecretConflictRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: []byte("v0")},
	})
	var conflicts int32
	k8s.PrependReactor("update", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if atomic.LoadInt32(&conflicts) < 2 {
			atomic.AddInt32(&conflicts, 1)
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "team-a-tokens", errors.New("simulated"))
		}
		return false, nil, nil
	})

	err := mutateSecret(context.Background(), k8s, "team-a", "team-a-tokens", TokensSecretKey,
		func(existing []byte) ([]byte, error) {
			return append(append([]byte{}, existing...), []byte("-mutated")...), nil
		})
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	if got := atomic.LoadInt32(&conflicts); got != 2 {
		t.Errorf("simulated conflicts = %d, want 2 (retry-then-succeed)", got)
	}

	// The final write must reflect the mutate applied to the freshly
	// re-read value — proving the loop converged rather than clobbering.
	secret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(secret.Data[TokensSecretKey]); got != "v0-mutated" {
		t.Errorf("secret value = %q, want %q", got, "v0-mutated")
	}
}

// TestMutateSecretConflictExhaustsRetries pins the terminal path: when
// every Update keeps conflicting, mutateSecret gives up after
// maxConflictRetries and surfaces the conflict rather than spinning.
func TestMutateSecretConflictExhaustsRetries(t *testing.T) {
	k8s := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a-tokens", Namespace: "team-a"},
		Data:       map[string][]byte{TokensSecretKey: []byte("v0")},
	})
	var calls int32
	k8s.PrependReactor("update", "secrets", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&calls, 1)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "team-a-tokens", errors.New("simulated"))
	})

	err := mutateSecret(context.Background(), k8s, "team-a", "team-a-tokens", TokensSecretKey,
		func(existing []byte) ([]byte, error) { return append(existing, 'x'), nil })
	if err == nil {
		t.Fatal("mutateSecret returned nil, want conflict error after retry budget")
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxConflictRetries) {
		t.Errorf("Update called %d times, want %d (full retry budget)", got, maxConflictRetries)
	}
}

// TestMutateSecretCreatesWhenAbsent covers the create branch: when the
// Secret does not exist and the mutate yields a non-empty payload,
// mutateSecret materializes a fresh Secret with the key set.
func TestMutateSecretCreatesWhenAbsent(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	err := mutateSecret(context.Background(), k8s, "team-a", "team-a-tokens", TokensSecretKey,
		func(existing []byte) ([]byte, error) {
			if len(existing) != 0 {
				t.Errorf("existing = %q, want empty on absent Secret", existing)
			}
			return []byte("created"), nil
		})
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	secret, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if got := string(secret.Data[TokensSecretKey]); got != "created" {
		t.Errorf("secret value = %q, want %q", got, "created")
	}
}

// TestMutateSecretAbsentStaysAbsentOnEmptyResult pins the no-write
// contract: a no-op mutate against an absent Secret must NOT materialize
// an empty Secret as a side effect. The refresh-store Revoke/Sweep paths
// depend on this so "absent stays absent" is observable.
func TestMutateSecretAbsentStaysAbsentOnEmptyResult(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	err := mutateSecret(context.Background(), k8s, "team-a", "team-a-tokens", TokensSecretKey,
		func([]byte) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("mutateSecret: %v", err)
	}
	if _, err := k8s.CoreV1().Secrets("team-a").Get(context.Background(), "team-a-tokens", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Secret unexpectedly created on empty-result no-op: %v", err)
	}
}
