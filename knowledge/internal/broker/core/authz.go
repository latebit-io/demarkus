package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNotAuthorized means the identity matched no world, or the addressed world's
// Allow rejected it. Tool handlers surface it as a descriptive tool error.
var ErrNotAuthorized = errors.New("broker: identity not authorized for any world")

// OIDCDomainAllowed is the broker wide hosted domain gate: an empty list
// admits every verified identity, otherwise the hd claim must be listed.
// Consumer accounts carry no hd and are refused.
func OIDCDomainAllowed(allowDomains []string, hd string) bool {
	if len(allowDomains) == 0 {
		return true
	}
	return slices.Contains(allowDomains, strings.ToLower(strings.TrimSpace(hd)))
}

// Identity gate refusals; each sign-in surface maps them to its own envelope.
var (
	ErrIdentityUnverified = errors.New("broker: email not verified")
	ErrIdentityDomain     = errors.New("broker: domain not permitted")
	ErrIdentityNoEmail    = errors.New("broker: email claim missing")
)

// GateIdentity is the one org gate for every path that admits or mints an
// identity. It canonicalizes claims.Email so stored and signed claims agree.
func GateIdentity(allowDomains []string, claims *Claims) error {
	if !claims.EmailVerified {
		return ErrIdentityUnverified
	}
	if !OIDCDomainAllowed(allowDomains, claims.HD) {
		return ErrIdentityDomain
	}
	claims.Email = CanonicalEmail(claims.Email)
	if claims.Email == "" {
		return ErrIdentityNoEmail
	}
	return nil
}

// authorizedWorlds is the writer set: the worlds whose Allow admits claims.
// Never use it for read discovery; reads pass the org gate alone
// (ReadableWorlds), and filtering by Allow would hide worlds from readers.
func authorizedWorlds(reg *WorldRegistry, claims *Claims) []WorldConfig {
	worlds := reg.All()
	out := make([]WorldConfig, 0, len(worlds))
	for j := range worlds {
		if WorldAllows(&worlds[j].Allow, claims) {
			out = append(out, worlds[j])
		}
	}
	return out
}

// ReadableWorlds is every world: reads pass the org gate alone, and the
// per world Allow is the writer list. A read restriction would filter here,
// never on Allow.
func ReadableWorlds(reg *WorldRegistry) []WorldConfig {
	return reg.All()
}

// AmbiguousTenantError is the deny-closed provisioning error for an identity
// matching several worlds. World names identify tenants, so Error() stays
// opaque for clients; the resolution site logs First/Second for the operator.
type AmbiguousTenantError struct {
	First, Second string
}

func (AmbiguousTenantError) Error() string {
	return "broker: identity maps to more than one world; contact the operator"
}

// TenantWorldFor resolves the single world a memory-broker identity owns
// (identity = world): zero matches is ErrNotAuthorized, two or more is a
// provisioning error and denies closed rather than guessing.
func TenantWorldFor(reg *WorldRegistry, issuer string, claims *Claims) (WorldConfig, error) {
	// Identity index first: a provisioned tenant owns its pinned slug. A
	// hit whose Allow rejects the caller means the record's email is
	// stale; fail so EnsureTenant's slow path refreshes it.
	if slug, ok := reg.SlugForIdentity(IdentityKey(issuer, claims.Subject)); ok {
		if w, found := reg.Find(slug); found && WorldAllows(&w.Allow, claims) {
			return w, nil
		}
		return WorldConfig{}, ErrNotAuthorized
	}
	var match *WorldConfig
	worlds := reg.All()
	for j := range worlds {
		w := &worlds[j]
		if !WorldAllows(&w.Allow, claims) {
			continue
		}
		if match != nil {
			return WorldConfig{}, AmbiguousTenantError{First: match.Name, Second: w.Name}
		}
		match = w
	}
	if match == nil {
		return WorldConfig{}, ErrNotAuthorized
	}
	return *match, nil
}

// ValidateTenantWorlds (memory broker, called after LoadConfig) requires
// every world to name its tenant: an empty Allow admits every identity,
// which in identity-=-world mode makes tenant resolution ambiguous.
func (c *Config) ValidateTenantWorlds() error {
	for i := range c.Worlds {
		a := &c.Worlds[i].Allow
		if len(a.Domains) == 0 && len(a.Groups) == 0 && len(a.Emails) == 0 {
			return fmt.Errorf("worlds[%d] (%s): allow must not be empty in a memory broker; each world is provisioned for one identity", i, c.Worlds[i].Name)
		}
	}
	return nil
}

// LookupWorld returns the configured world (static or dynamic) with the
// given name, or nil when none matches. Shared by the MCP write gate and
// the per-world write-token store so both resolve names identically.
func LookupWorld(reg *WorldRegistry, name string) *WorldConfig {
	if w, ok := reg.Find(name); ok {
		return &w
	}
	return nil
}

// WorldAllows is the AllowConfig predicate: no lists means everyone, a
// listed email always passes, otherwise domains and groups both apply
// (an empty one is no restriction) and an emails only list refuses the rest.
func WorldAllows(a *AllowConfig, claims *Claims) bool {
	if len(a.Domains) == 0 && len(a.Groups) == 0 && len(a.Emails) == 0 {
		return true
	}
	if emailMatches(claims.Email, a.Emails) {
		return true
	}
	if len(a.Domains) == 0 && len(a.Groups) == 0 {
		return false
	}
	return domainMatches(claims.Email, a.Domains) && groupsMatch(claims.Groups, a.Groups)
}

// emailMatches reports whether the lowercased+trimmed identity email is
// in the (already-normalized at config load) Emails list.
func emailMatches(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return false
	}
	return slices.Contains(allowed, CanonicalEmail(email))
}

// domainMatches admits an email whose domain is listed; an empty list is no
// restriction. The list is lowercased at config load, so a plain compare
// suffices.
func domainMatches(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	return slices.Contains(allowed, domain)
}

// groupsMatch admits an identity with one listed group; an empty list is no
// restriction. Case insensitive (see AllowConfig.Groups); an IdP that sends
// no groups fails the match, and Emails is the operator's carve out.
func groupsMatch(have, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, g := range have {
		if slices.Contains(allowed, strings.ToLower(g)) {
			return true
		}
	}
	return false
}
