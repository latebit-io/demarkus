package core

import (
	"errors"
	"testing"
)

// TestReadableWorldsIgnoresWriterAllow pins the read/write split: reads pass
// the SSO org gate alone, so every world is readable whatever its writer
// Allow. The writer set is TestWorldAllowsPredicate.
func TestReadableWorldsIgnoresWriterAllow(t *testing.T) {
	cfg := testConfig() // team-a: Allow domains=example.com
	cfg.Worlds = append(cfg.Worlds, WorldConfig{
		Name:         "locked",
		Namespace:    "locked",
		TokensSecret: "locked-tokens",
		Allow:        AllowConfig{Emails: []string{"only-admin@nowhere.test"}},
	})
	got := ReadableWorlds(cfg.Registry())
	if len(got) != 2 {
		t.Fatalf("ReadableWorlds = %d, want 2 (a restrictive writer Allow must not hide a world from readers)", len(got))
	}
	seen := map[string]bool{}
	for _, w := range got {
		seen[w.Name] = true
	}
	if !seen["team-a"] || !seen["locked"] {
		t.Errorf("ReadableWorlds names = %v, want both team-a and locked", seen)
	}
}

// TestWorldAllowsPredicate is the writer authorization table: the MCP write
// gate and /me/install both reduce to this predicate.
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
			// list. WorldAllows must accept regardless.
			allow:  AllowConfig{Groups: []string{"engineering"}, Emails: []string{"contractor@example.com"}},
			claims: Claims{Email: "contractor@example.com", EmailVerified: true},
			accept: true,
		},
		{
			name: "emails_only_no_match_rejects",
			// Emails only must not fall through to the empty domains and
			// groups branch, or "emails: [alice]" would mean everyone plus alice.
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
			if got := WorldAllows(&tt.allow, &tt.claims); got != tt.accept {
				t.Errorf("WorldAllows = %v, want %v", got, tt.accept)
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
		got := authorizedWorlds(cfg.Registry(), &Claims{Email: "alice@example.com", EmailVerified: true})
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
		got := authorizedWorlds(cfg.Registry(), &Claims{Email: "anyone@anywhere.test", EmailVerified: true})
		if len(got) != 1 || got[0].Name != "team-a" {
			t.Errorf("authorizedWorlds = %+v, want team-a admitted on empty allowlist", got)
		}
	})

	t.Run("unauthorized_identity_gets_none", func(t *testing.T) {
		cfg := testConfig()
		got := authorizedWorlds(cfg.Registry(), &Claims{Email: "mallory@evil.example", EmailVerified: true})
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
		got := authorizedWorlds(cfg.Registry(), &Claims{Email: "alice@example.com", EmailVerified: true})
		if len(got) != 2 || got[0].Name != "team-a" || got[1].Name != "team-b" {
			t.Errorf("authorizedWorlds order = %+v, want [team-a, team-b]", got)
		}
	})
}

// TestLookupWorld pins the name resolver shared by the MCP write gate
// and the per-world write-token store: exact-name match or nil.
func TestLookupWorld(t *testing.T) {
	cfg := testConfig()
	if w := LookupWorld(cfg.Registry(), "team-a"); w == nil || w.Name != "team-a" {
		t.Errorf("LookupWorld(team-a) = %+v, want team-a", w)
	}
	if w := LookupWorld(cfg.Registry(), "nope"); w != nil {
		t.Errorf("LookupWorld(nope) = %+v, want nil", w)
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
			got := OIDCDomainAllowed(tt.allow, tt.hd)
			if got != tt.accept {
				t.Errorf("OIDCDomainAllowed(%v, %q) = %v, want %v", tt.allow, tt.hd, got, tt.accept)
			}
		})
	}
}

func TestGateIdentity(t *testing.T) {
	tests := []struct {
		name         string
		allowDomains []string
		claims       Claims
		wantErr      error
		wantEmail    string
	}{
		{"verified, no domain list", nil, Claims{Email: " Alice@Example.COM ", EmailVerified: true}, nil, "alice@example.com"},
		{"verified, allowed domain", []string{"example.com"}, Claims{Email: "a@example.com", EmailVerified: true, HD: "Example.com"}, nil, "a@example.com"},
		{"unverified email", nil, Claims{Email: "a@example.com"}, ErrIdentityUnverified, ""},
		{"foreign domain", []string{"example.com"}, Claims{Email: "a@evil.com", EmailVerified: true, HD: "evil.com"}, ErrIdentityDomain, ""},
		{"blank email", nil, Claims{Email: "  ", EmailVerified: true}, ErrIdentityNoEmail, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := tt.claims
			err := GateIdentity(tt.allowDomains, &claims)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("GateIdentity() = %v, want %v", err, tt.wantErr)
			}
			if err == nil && claims.Email != tt.wantEmail {
				t.Errorf("email = %q, want canonical %q", claims.Email, tt.wantEmail)
			}
		})
	}
}
