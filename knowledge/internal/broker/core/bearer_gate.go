package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/latebit-io/demarkus/protocol"
)

// BearerGate admits mark requests that carry an identity bearer in the auth
// field (ADR 0033): the gateways' bearer check and audience, then the
// world's tenancy and write predicates, decided per request.
type BearerGate struct {
	check  BearerCheck
	issuer string
	worlds *WorldRegistry
	// resources is each profile's gateway resource, the audience its worlds take.
	resources map[string]string
	log       *slog.Logger
}

// NewBearerGate builds the gate over cfg's worlds with the verifier the
// gateways share.
func NewBearerGate(cfg *Config, verifier Verifier, log *slog.Logger) *BearerGate {
	return &BearerGate{
		check:  BearerCheck{Verifier: verifier, AllowDomains: cfg.OIDC.AllowDomains},
		issuer: cfg.OIDC.Issuer,
		worlds: cfg.Registry(),
		resources: map[string]string{
			ProfileKnowledge: cfg.Server.Gateway(ProfileKnowledge).Resource(),
			ProfileMemory:    cfg.Server.Gateway(ProfileMemory).Resource(),
		},
		log: log,
	}
}

// For is the gate of one connection to authority. The local world is found
// once; each request reads its current allow and write scope.
func (b *BearerGate) For(authority string) (protocol.Gate, error) {
	name, ok := b.worlds.LocalName(authority)
	if !ok {
		return nil, fmt.Errorf("no local world has authority %q", authority)
	}
	return func(ctx context.Context, req protocol.Request) (protocol.Grant, error) {
		grant, err := b.admit(ctx, name, req.Metadata["auth"])
		if err != nil {
			b.log.InfoContext(ctx, "broker: bearer refused", "world", name, "err", err)
		}
		return grant, err
	}, nil
}

// errNoBearer refuses a request with an empty auth field: the bearer
// listener serves no anonymous reads.
var errNoBearer = errors.New("bearer required in the auth field")

func (b *BearerGate) admit(ctx context.Context, worldName, bearer string) (protocol.Grant, error) {
	if bearer == "" {
		return protocol.Grant{}, errNoBearer
	}
	w, ok := b.worlds.Find(worldName)
	if !ok {
		// Deprovisioned while the connection stayed open.
		return protocol.Grant{}, fmt.Errorf("%w: world %q is gone", protocol.ErrNotPermitted, worldName)
	}
	claims, err := b.check.Authenticate(ctx, b.resources[w.Profile], bearer)
	if err != nil {
		return protocol.Grant{}, fmt.Errorf("subject %s: %w", HashSubject(claims.Subject), err)
	}
	if w.Profile == ProfileMemory {
		// The memory gateway's tenancy: the identity's one world must be this one.
		tenant, err := TenantWorldFor(b.worlds.View(ProfileMemory), b.issuer, &claims)
		if err != nil || tenant.Name != w.Name {
			return protocol.Grant{}, fmt.Errorf("%w: subject %s is not the tenant of %q", protocol.ErrNotPermitted, HashSubject(claims.Subject), w.Name)
		}
	}
	grant, err := WriteGrant(&w, &claims)
	if err != nil {
		// Knowledge reads pass the org gate alone; outside allow, no paths.
		grant = protocol.Grant{Label: claims.Email, Expires: claims.Expiry}
	}
	return grant, nil
}
