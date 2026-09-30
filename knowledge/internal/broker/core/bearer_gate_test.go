package core

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

// bearerGateFixture serves a knowledge world, a tenant and a remote world;
// each bearer names the identity the fake verifier returns for it.
func bearerGateFixture(t *testing.T) (*Config, *BearerGate, time.Time) {
	t.Helper()
	expiry := time.Date(2026, 9, 30, 12, 15, 0, 0, time.UTC)
	cfg := &Config{
		Server: ServerConfig{
			PublicURL: "https://broker.example.com",
			Memory:    GatewayConfig{PublicURL: "https://memory.example.com"},
		},
		Worlds: []WorldConfig{
			{
				Name: "team-a", Profile: ProfileKnowledge, Local: true, InternalAddress: "team-a.example.com:6309",
				Allow: AllowConfig{Domains: []string{"example.com"}}, WriteScope: WriteScope{Paths: []string{"/team-a/**"}},
			},
			{
				Name: "alice-w", Profile: ProfileMemory, Local: true, InternalAddress: "alice-w.tenants.example.com:6309",
				Allow: AllowConfig{Emails: []string{"alice@example.com"}}, WriteScope: WriteScope{Paths: []string{"/**"}},
			},
			{Name: "far", Profile: ProfileKnowledge, InternalAddress: "far.example.com:6309"},
		},
	}
	identities := map[string]Claims{
		"alice":        {Subject: "idp|alice", Email: "Alice@Example.com", EmailVerified: true, Expiry: expiry},
		"bob":          {Subject: "idp|bob", Email: "bob@example.com", EmailVerified: true, Expiry: expiry},
		"eve":          {Subject: "idp|eve", Email: "eve@other.test", EmailVerified: true, Expiry: expiry},
		"unverified":   {Subject: "idp|mallory", Email: "mallory@example.com"},
		"alice-memory": {Subject: "idp|alice", Email: "alice@example.com", EmailVerified: true, Resource: "https://memory.example.com/mcp"},
	}
	verifier := &fakeVerifier{VerifyFn: func(raw string) (Claims, error) {
		if claims, ok := identities[raw]; ok {
			return claims, nil
		}
		return Claims{}, errors.New("signature invalid")
	}}
	return cfg, NewBearerGate(cfg, verifier, slog.New(slog.DiscardHandler)), expiry
}

func admitWith(t *testing.T, gate protocol.Gate, bearer string) (protocol.Grant, error) {
	t.Helper()
	req := protocol.Request{Verb: protocol.VerbFetch, Path: "/index.md", Metadata: map[string]string{}}
	if bearer != "" {
		req.Metadata["auth"] = bearer
	}
	return gate(context.Background(), req)
}

func TestBearerGateFindsLocalWorldsByAuthority(t *testing.T) {
	_, gates, _ := bearerGateFixture(t)
	if _, err := gates.For("TEAM-A.example.com"); err != nil {
		t.Fatalf("For(team-a, any case) = %v, want a gate", err)
	}
	for _, authority := range []string{"far.example.com", "unknown.example.com"} {
		if _, err := gates.For(authority); err == nil {
			t.Errorf("For(%s) built a gate; only local worlds have one", authority)
		}
	}
}

func TestBearerGateOnAKnowledgeWorld(t *testing.T) {
	_, gates, expiry := bearerGateFixture(t)
	gate, err := gates.For("team-a.example.com")
	if err != nil {
		t.Fatal(err)
	}

	grant, err := admitWith(t, gate, "alice")
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	want := protocol.Grant{Label: "alice@example.com", Paths: []string{"/team-a/**"}, Expires: expiry}
	if grant.Label != want.Label || !slices.Equal(grant.Paths, want.Paths) || !grant.Expires.Equal(want.Expires) {
		t.Errorf("alice's grant = %+v, want %+v", grant, want)
	}

	grant, err = admitWith(t, gate, "eve")
	if err != nil {
		t.Fatalf("eve reads a knowledge world past the org gate: %v", err)
	}
	if grant.Label != "eve@other.test" || len(grant.Paths) != 0 || !grant.Expires.Equal(expiry) {
		t.Errorf("eve's grant = %+v, want a read only grant with her expiry", grant)
	}

	for _, bearer := range []string{"", "forged", "unverified", "alice-memory"} {
		_, err := admitWith(t, gate, bearer)
		if err == nil || errors.Is(err, protocol.ErrNotPermitted) {
			t.Errorf("bearer %q: err = %v, want an unauthorized refusal", bearer, err)
		}
	}
}

func TestBearerGateOnATenantWorld(t *testing.T) {
	_, gates, _ := bearerGateFixture(t)
	gate, err := gates.For("alice-w.tenants.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, bearer := range []string{"alice", "alice-memory"} {
		grant, err := admitWith(t, gate, bearer)
		if err != nil || !slices.Equal(grant.Paths, []string{"/**"}) {
			t.Errorf("tenant %q: grant %+v, err %v, want the tenant's scope", bearer, grant, err)
		}
	}
	if _, err := admitWith(t, gate, "eve"); !errors.Is(err, protocol.ErrNotPermitted) {
		t.Errorf("another identity on a tenant's world: err = %v, want ErrNotPermitted", err)
	}
}

// Tenancy is the memory gateway's: an identity two tenant worlds admit owns
// neither, on the wire as over MCP.
func TestBearerGateRefusesAnAmbiguousTenant(t *testing.T) {
	cfg, gates, _ := bearerGateFixture(t)
	twin := WorldConfig{
		Name: "alice-twin", Profile: ProfileMemory, Local: true, InternalAddress: "alice-twin.tenants.example.com:6309",
		Allow: AllowConfig{Emails: []string{"alice@example.com"}},
	}
	cfg.Registry().SetDynamic([]WorldConfig{twin}, nil)
	gate, err := gates.For("alice-w.tenants.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitWith(t, gate, "alice"); !errors.Is(err, protocol.ErrNotPermitted) {
		t.Errorf("an identity two tenant worlds admit: err = %v, want ErrNotPermitted", err)
	}
}

func TestBearerGateAppliesTheOrgGate(t *testing.T) {
	cfg, _, _ := bearerGateFixture(t)
	cfg.OIDC.AllowDomains = []string{"example.com"}
	identities := &fakeVerifier{Claims: Claims{Subject: "idp|alice", Email: "alice@example.com", EmailVerified: true}}
	gate, err := NewBearerGate(cfg, identities, slog.New(slog.DiscardHandler)).For("team-a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admitWith(t, gate, "any"); err == nil || errors.Is(err, protocol.ErrNotPermitted) {
		t.Errorf("no hosted domain claim under allowDomains: err = %v, want unauthorized", err)
	}
}

func TestBearerGateReadsTheRegistryPerRequest(t *testing.T) {
	cfg, gates, _ := bearerGateFixture(t)
	bob := WorldConfig{
		Name: "bob-w", Profile: ProfileMemory, Local: true, InternalAddress: "bob-w.tenants.example.com:6309",
		Allow: AllowConfig{Emails: []string{"bob@example.com"}}, WriteScope: WriteScope{Paths: []string{"/**"}},
	}
	cfg.Registry().SetDynamic([]WorldConfig{bob}, nil)
	gate, err := gates.For("bob-w.tenants.example.com")
	if err != nil {
		t.Fatalf("a provisioned tenant has a gate: %v", err)
	}
	if _, err := admitWith(t, gate, "bob"); err != nil {
		t.Fatalf("before deprovision: %v", err)
	}
	cfg.Registry().SetDynamic(nil, nil)
	if _, err := admitWith(t, gate, "bob"); !errors.Is(err, protocol.ErrNotPermitted) {
		t.Errorf("after deprovision on an open connection: err = %v, want ErrNotPermitted", err)
	}
}
