package broker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// TokensSecretKey is the key in a world's tokens Secret holding the
	// tokens.toml payload the world server reads.
	TokensSecretKey = "tokens.toml"

	// maxConflictRetries bounds the read-modify-write loop on Secret
	// resourceVersion conflicts. Five gives multi-replica brokers plenty
	// of room to converge under any realistic contention while still
	// failing the request promptly on a stuck conflict.
	maxConflictRetries = 5
)

// ErrNotAuthorized indicates the identity matched zero worlds during
// authorization, or the addressed world's Allow predicate rejected the
// caller. The MCP federation/read handlers surface it with a
// descriptive tool-error message.
var ErrNotAuthorized = errors.New("broker: identity not authorized for any world")

// oidcDomainAllowed reports whether the IdP-issued identity satisfies
// the broker-global hosted-domain allowlist (OIDC.AllowDomains). An
// empty allowlist disables the gate — any verified identity passes
// the broker layer (per-world Allow lists still apply downstream).
// When non-empty, the identity's hd claim (Google Workspace
// hosted-domain binding, set server-side by Google from the Workspace
// tenant) must be in the list. Consumer Google accounts have no hd
// claim and are rejected. The allowlist is lowercased+trimmed at
// config load, so a plain Contains against the lowercased hd is
// sufficient on the hot path.
func oidcDomainAllowed(allowDomains []string, hd string) bool {
	if len(allowDomains) == 0 {
		return true
	}
	return slices.Contains(allowDomains, strings.ToLower(strings.TrimSpace(hd)))
}

// authorizedWorlds returns the configured worlds whose Allow predicate
// admits claims — the WRITER set. Used by /auth/callback and /me/install
// (which wire writable worlds into a plugin) and mirrored by the MCP write
// gate. WorldConfig.Allow is the writer allowlist; SSO at the broker is the
// org gate. A pure read over config, no Secret writes.
//
// Do NOT use this for read-discovery (mark_worlds) — reads are gated by the
// SSO org gate alone, not Allow (a world's tokens.toml grants no read op), so
// filtering the world LIST by the writer allowlist wrongly hides readable
// worlds from non-writers. Use readableWorlds for that.
func authorizedWorlds(cfg *Config, claims *Claims) []*WorldConfig {
	out := make([]*WorldConfig, 0, len(cfg.Worlds))
	for j := range cfg.Worlds {
		w := &cfg.Worlds[j]
		if worldAllows(&w.Allow, claims) {
			out = append(out, w)
		}
	}
	return out
}

// readableWorlds returns the worlds an authenticated identity may READ.
// Reads are gated only by the broker's SSO/allowDomains org gate — not by
// per-world Allow (that is the writer allowlist) — so every configured world
// is readable by any identity that cleared the broker login gate. This is the
// read-discovery seam mark_worlds lists from, kept separate from the writer
// Allow so "all logins read everything, writes are allow-listed" is
// expressible: opening reads here never widens writes. Per-world READ
// restrictions (Phase 2 restricted collections) would filter here, on a
// dedicated read predicate, never on the write Allow.
func readableWorlds(cfg *Config) []*WorldConfig {
	out := make([]*WorldConfig, 0, len(cfg.Worlds))
	for j := range cfg.Worlds {
		out = append(out, &cfg.Worlds[j])
	}
	return out
}

// lookupWorld returns the configured world with the given name, or nil
// when none matches. Shared by the MCP write gate and the per-world
// write-token store so the two layers resolve world names identically.
func lookupWorld(cfg *Config, name string) *WorldConfig {
	for j := range cfg.Worlds {
		if cfg.Worlds[j].Name == name {
			return &cfg.Worlds[j]
		}
	}
	return nil
}

// worldAllows is the AllowConfig predicate. See the AllowConfig doc on
// config.go for the human-readable rule; the order of checks here is:
//
//  1. All three lists empty → match (back-compat for worlds with no
//     allowlist configured, same as the pre-Slice-C behavior).
//  2. Email-in-Emails → match (per-user carve-out bypasses both
//     domain and group requirements).
//  3. Domains+Groups predicate, where an empty list on either dimension
//     means "no restriction on that dimension." But if only Emails was
//     set and Step 2 didn't match, Step 3 must reject — otherwise
//     `emails: [alice@x]` with no other allowlist would silently mean
//     "everyone plus alice." Detected as `len(Domains)+len(Groups)==0`
//     when we get here.
func worldAllows(a *AllowConfig, claims *Claims) bool {
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
	want := strings.ToLower(strings.TrimSpace(email))
	return slices.Contains(allowed, want)
}

// domainMatches reports whether the identity email's domain part is in
// the allowlist. Returns true on an empty allowlist — the "no
// restriction on the domain dimension" semantic worldAllows depends on.
//
// The allowlist is lowercased+trimmed at config load (validate in
// config.go), so a plain compare against the lowercased domain part of
// the email is sufficient — no EqualFold needed on the hot path.
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

// groupsMatch reports whether the identity has at least one group in
// the allowlist. Returns true on an empty allowlist — the "no
// restriction on the group dimension" semantic worldAllows depends on.
// IdPs that don't surface groups in the ID token send a nil claims.Groups;
// in that case the match fails (unless the allowlist is also empty),
// which is the operator's signal to use AllowEmails as a carve-out.
//
// Match is case-insensitive: the allowlist is lowercased at config load
// (validate in config.go) and the claim's groups are lowercased here.
// See the AllowConfig.Groups doc for why our target IdPs (Google, Okta,
// Entra ID, Auth0) make case-insensitive the safe default.
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

// mutateSecret performs an optimistic-concurrency read-modify-write on
// the Secret data[key]. If the Secret does not exist, it is created with
// the named key set to the mutate-result on empty input. Retries
// resourceVersion conflicts up to maxConflictRetries; surfaces all other
// errors immediately.
//
// Free function rather than a method on any one type so the broker's
// refreshStore and worldWriteTokenStore can share the same
// Secret-mutation contract without duplicating the conflict-retry loop.
// See /guidelines.md "Don't Duplicate Logic".
func mutateSecret(ctx context.Context, k8s kubernetes.Interface, namespace, name, key string, mutate func([]byte) ([]byte, error)) error {
	for range maxConflictRetries {
		secret, getErr := k8s.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil && !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("get secret %s/%s: %w", namespace, name, getErr)
		}
		var existing []byte
		if getErr == nil {
			existing = secret.Data[key]
		}
		next, err := mutate(existing)
		if err != nil {
			return err
		}
		if apierrors.IsNotFound(getErr) {
			// No-op closure on an absent Secret: don't materialize an
			// empty Secret as a side effect. Refresh-store Revoke and
			// Sweep both return existing-unchanged when there's
			// nothing to do; without this guard the first such call
			// would create a `{key: nil}` Secret that the next call
			// then has to read back. The no-write contract is
			// observable (helm-rendered Secrets, audit logs), so
			// preserving "absent stays absent" is part of the
			// store's behavior, not an internal detail.
			if len(next) == 0 {
				return nil
			}
			fresh := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{key: next},
			}
			_, createErr := k8s.CoreV1().Secrets(namespace).Create(ctx, fresh, metav1.CreateOptions{})
			if createErr == nil {
				return nil
			}
			if apierrors.IsAlreadyExists(createErr) {
				continue
			}
			return fmt.Errorf("create secret %s/%s: %w", namespace, name, createErr)
		}
		// No-op mutation: the value is unchanged, so skip the Update.
		// Writing it anyway bumps the Secret's resourceVersion, which
		// triggers a kubelet re-projection on every world tokens.toml
		// mount and amplifies conflict retries across replicas — the
		// exact propagation churn the broker works to avoid.
		if bytes.Equal(existing, next) {
			return nil
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte, 1)
		}
		secret.Data[key] = next
		_, updateErr := k8s.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
		if updateErr == nil {
			return nil
		}
		if apierrors.IsConflict(updateErr) {
			continue
		}
		return fmt.Errorf("update secret %s/%s: %w", namespace, name, updateErr)
	}
	return fmt.Errorf("conflict on secret %s/%s after %d retries", namespace, name, maxConflictRetries)
}
