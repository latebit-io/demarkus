package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/latebit/demarkus/protocol/token"
)

const (
	// TokensSecretKey is the key in the world tokens Secret holding the
	// tokens.toml payload the server reads.
	TokensSecretKey = "tokens.toml"
	// IssuancesSecretKey is the key in the broker's issuances Secret
	// holding the JSON map of label → ownership metadata.
	IssuancesSecretKey = "issuances.json"

	// maxConflictRetries bounds the read-modify-write loop on Secret
	// resourceVersion conflicts. Five gives multi-replica brokers plenty
	// of room to converge under any realistic contention while still
	// failing the request promptly on a stuck conflict.
	maxConflictRetries = 5
	// maxLabelRetries bounds the rare case where two simultaneous mints
	// pick the same 32-bit opaque label. With 2^32 space and few
	// outstanding labels, collisions are statistically negligible — five
	// retries reduces the per-mint failure probability to ~zero.
	maxLabelRetries = 5
)

// MintResult is one entry of the broker's /auth/callback response. RawToken
// is the one-time plaintext value returned to the caller; the broker
// never stores it.
type MintResult struct {
	World     string    `json:"world"`
	Label     string    `json:"label"`
	RawToken  string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Issuance is the broker-side record stored in the issuances Secret. Raw
// token material is never recorded here; only the hash-on-disk (in the
// world Secret) and the ownership metadata (here) exist after Mint
// returns. Together they let the broker authorize List/Revoke without
// the world server learning anything about identity.
type Issuance struct {
	Label      string    `json:"label"`
	Email      string    `json:"email"`
	World      string    `json:"world"`
	Paths      []string  `json:"paths"`
	Operations []string  `json:"operations"`
	IssuedAt   time.Time `json:"issuedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Issuances is the top-level shape of the broker's issuances Secret. A
// flat slice is fine for Slice B; an email-secondary index lives in the
// backlog (plan §6.2 cleanup) for fast revoke-all-for-email queries.
type Issuances struct {
	Entries []Issuance `json:"entries"`
}

// Issuer is the broker's token-issuance engine. It owns the
// kubernetes.Interface used to read and write Secrets; tests pass a
// fake.Clientset and exercise the same code path real deployments do.
//
// Clock is exposed so tests can pin issuance timestamps without sleeping.
// All other dependencies (config, k8s client) are immutable after
// NewIssuer returns.
type Issuer struct {
	cfg      *Config
	k8s      kubernetes.Interface
	clock    func() time.Time
	labelGen func() (string, error)
}

// NewIssuer wires the issuer with a real time.Now clock and the default
// random label generator. Tests override clock and labelGen on the
// returned struct to exercise collision and time-based behavior.
func NewIssuer(cfg *Config, k8s kubernetes.Interface) *Issuer {
	return &Issuer{cfg: cfg, k8s: k8s, clock: time.Now, labelGen: NewLabel}
}

// ErrNotAuthorized indicates the identity matched zero worlds during
// authorization. Callers should surface this as HTTP 403.
var ErrNotAuthorized = errors.New("broker: identity not authorized for any world")

// ErrNotOwner indicates the caller is trying to revoke or list a label
// that belongs to a different identity. HTTP 403 too.
var ErrNotOwner = errors.New("broker: caller is not the token owner")

// ErrNotFound indicates a label has no issuance record. HTTP 404.
var ErrNotFound = errors.New("broker: label not found in issuances")

// Mint issues one token per world the caller qualifies for. If zero
// worlds authorize the caller, returns ErrNotAuthorized and no Secret
// is written. On partial failure (some worlds succeed, then one fails)
// the partial results are returned alongside the error so the caller can
// hand back what was actually minted — the unfinished worlds are not
// reflected in either Secret.
func (i *Issuer) Mint(ctx context.Context, claims Claims) ([]MintResult, error) {
	// Defense in depth: the production Verifier rejects unverified claims
	// before Mint is reached, but a test double (or future Verifier impl)
	// might not. Authorization by email domain is meaningless on an
	// unverified address.
	if !claims.EmailVerified {
		return nil, ErrEmailUnverified
	}
	// Canonicalize the email once at the boundary: trim + lowercase, then
	// reject empty. Reasons to do this here, not in the leaf helpers:
	//
	//  - An untrimmed email (e.g. "alice@example.com ") evades
	//    domainMatches because LastIndex("@") splits the local-part from
	//    the domain *including the trailing whitespace*, and the
	//    config-side allowlist is trim+lowercase normalized.
	//  - The canonicalized value is what lands in the issuances Secret
	//    as the owner. Without normalization here, two logins of the
	//    same user with different IdP-side casing produce two distinct
	//    "owners" for List/Revoke, even though EqualFold compares paper
	//    over the difference on the read path.
	//  - The empty-email guard prevents the empty string from becoming a
	//    shared "no identity" owner across every future no-email caller,
	//    in the path where every Allow list is empty (back-compat
	//    "any verified user" worlds).
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	if claims.Email == "" {
		return nil, ErrNotAuthorized
	}
	worlds := i.authorizedWorlds(claims)
	if len(worlds) == 0 {
		return nil, ErrNotAuthorized
	}
	now := i.clock()
	results := make([]MintResult, 0, len(worlds))
	for _, w := range worlds {
		r, err := i.mintForWorld(ctx, w, claims, now, w.DefaultToken.Paths, w.DefaultToken.Operations)
		if err != nil {
			return results, fmt.Errorf("mint for world %s: %w", w.Name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func (i *Issuer) authorizedWorlds(claims Claims) []*WorldConfig {
	out := make([]*WorldConfig, 0, len(i.cfg.Worlds))
	for j := range i.cfg.Worlds {
		w := &i.cfg.Worlds[j]
		if worldAllows(&w.Allow, claims) {
			out = append(out, w)
		}
	}
	return out
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
func worldAllows(a *AllowConfig, claims Claims) bool {
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

// mintForWorld issues one token against w with the given scope
// (paths + operations) and the world's current expiry policy. Slice
// B's Mint passes the world's DefaultToken scope; Slice C.3's
// RotateLabel passes the issuance-recorded scope, deliberately
// ignoring config-side scope changes between mint and rotate (see
// plan §Routes — operator scope changes only take effect on next
// `demarkus login`). Lifetime always uses the operator's current
// DefaultToken.ExpiresAfter so config-side expiry tightening DOES
// take effect on rotate; the asymmetry is intentional.
func (i *Issuer) mintForWorld(ctx context.Context, w *WorldConfig, claims Claims, now time.Time, paths, operations []string) (MintResult, error) {
	for range maxLabelRetries {
		label, err := i.labelGen()
		if err != nil {
			return MintResult{}, err
		}
		minted, err := token.Generate(label, paths, operations)
		if err != nil {
			return MintResult{}, fmt.Errorf("generate token: %w", err)
		}
		expiresAt := now.Add(w.DefaultToken.ExpiresAfter)
		minted.Entry.Expires = expiresAt.UTC().Format(time.RFC3339)

		err = i.appendToWorldSecret(ctx, w, label, &minted.Entry)
		if errors.Is(err, token.ErrLabelExists) {
			// Retry with a fresh label. After maxLabelRetries
			// consecutive collisions we fall out of the loop and
			// return the trailing error below.
			continue
		}
		if err != nil {
			return MintResult{}, err
		}
		issuance := Issuance{
			Label:      label,
			Email:      claims.Email,
			World:      w.Name,
			Paths:      append([]string(nil), paths...),
			Operations: append([]string(nil), operations...),
			IssuedAt:   now,
			ExpiresAt:  expiresAt,
		}
		if err := i.appendIssuance(ctx, &issuance); err != nil {
			// World secret already activated the token; without an
			// issuance record the broker can't revoke or sweep it.
			// Best-effort rollback so we don't leave an active
			// untracked token. Detach from the caller's cancelable
			// context: if the request was canceled (client hung up),
			// the rollback still needs to land. Bounded timeout so a
			// stuck k8s API doesn't wedge the handler.
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			rbErr := i.removeFromWorldSecret(rbCtx, w, label)
			cancel()
			// Cross-world label collision: another world already
			// owns this label in the issuances Secret. Treat as a
			// collision and retry with a fresh label (rollback
			// undid the world-secret write, so the loop is clean).
			if rbErr == nil && errors.Is(err, token.ErrLabelExists) {
				continue
			}
			if rbErr != nil {
				return MintResult{}, fmt.Errorf("record issuance: %w (rollback failed: %v)", err, rbErr)
			}
			return MintResult{}, fmt.Errorf("record issuance: %w", err)
		}
		return MintResult{
			World:     w.Name,
			Label:     label,
			RawToken:  minted.Raw,
			ExpiresAt: expiresAt,
		}, nil
	}
	return MintResult{}, fmt.Errorf("%d consecutive label collisions", maxLabelRetries)
}

// List returns the issuances owned by the given email. Pure read — no
// authorization check beyond the email match (the HTTP layer is
// responsible for ensuring the caller authenticated as that email).
func (i *Issuer) List(ctx context.Context, email string) ([]Issuance, error) {
	all, err := i.readIssuances(ctx)
	if err != nil {
		return nil, err
	}
	var out []Issuance
	for j := range all.Entries {
		if strings.EqualFold(all.Entries[j].Email, email) {
			out = append(out, all.Entries[j])
		}
	}
	return out, nil
}

// Revoke removes a label from both the world's tokens Secret and the
// broker's issuances Secret. Returns ErrNotFound if the label has no
// issuance record, ErrNotOwner if the caller's email does not match the
// issuance. User-initiated revocation path; the sweeper uses
// revokeIssuance directly (no owner check, already holds the entry).
func (i *Issuer) Revoke(ctx context.Context, callerEmail, label string) error {
	all, err := i.readIssuances(ctx)
	if err != nil {
		return err
	}
	var found *Issuance
	for j := range all.Entries {
		if all.Entries[j].Label == label {
			found = &all.Entries[j]
			break
		}
	}
	if found == nil {
		return ErrNotFound
	}
	if !strings.EqualFold(found.Email, callerEmail) {
		return ErrNotOwner
	}
	return i.revokeIssuance(ctx, found)
}

// RotateLabel issues a fresh token replacing label, returning the new
// MintResult. Slice C.3.
//
// Authorization model (see plan §Routes "rotate"):
//
//  1. Owner check — caller's claims.Email must match the issuance's
//     stored email. Same gate as Revoke; prevents one user from
//     rotating another's token.
//  2. Re-validation — the world's current Allow predicate must still
//     accept the caller. A user removed from the world's allowlist
//     between mint and rotate must NOT be able to extend access via
//     rotation. This is the difference between rotation-as-refresh
//     (would skip this check) and rotation-as-relogin (this
//     implementation). The bearer ID-token verification at the HTTP
//     layer already proves the IdP identity is still active; we add
//     the per-world predicate to catch in-org permission changes.
//
// Scope vs. lifetime asymmetry:
//
//   - Scope (paths + operations) stays frozen to the issuance record.
//     Operator narrowing of DefaultToken.Paths between mint and rotate
//     does NOT shrink the rotated token's reach; the user re-logs in to
//     pick up new scope.
//   - Lifetime resets to now + DefaultToken.ExpiresAfter. Operator
//     tightening of expiry DOES apply on rotate — the operator's
//     security tightening trumps the user's lifetime expectation.
//
// Sequence: mint new first, then revoke old. On revoke failure the
// new token is returned anyway and the soft error wraps the revoke
// failure — the sweeper catches the orphan on expiry. Picked over
// revoke-then-mint because the alternative leaves the user with no
// token if mint then fails (which forces a full re-login), worse UX
// than the briefly-two-valid-tokens window.
//
// Returns:
//   - (minted, nil) on full success.
//   - (zero MintResult, ErrNotFound/ErrNotOwner/ErrNotAuthorized) on
//     hard authz failures, BEFORE any state change.
//   - (zero MintResult, other err) on hard mint failures, also no
//     state change.
//   - (minted, err) when new mint succeeded but old revoke failed.
//     Callers detect via `result.Label != ""` and treat as 200 with a
//     warn log. Matches the Slice B partial-mint convention.
func (i *Issuer) RotateLabel(ctx context.Context, claims Claims, label string) (MintResult, error) {
	// Defense in depth: mirror Mint's email-verified gate. The
	// production Verifier rejects unverified ID tokens before they
	// reach this layer, but a future Verifier impl (or a test
	// double) might not — and rotation extends access, so the cost
	// of an unverified-identity rotation slipping through is exactly
	// the same as the original Mint footgun this guards against.
	if !claims.EmailVerified {
		return MintResult{}, ErrEmailUnverified
	}
	// Same canonicalization as Mint — the persisted issuance email
	// is lowercase+trim, so the owner-check compare needs the
	// caller's email in the same form.
	claims.Email = strings.ToLower(strings.TrimSpace(claims.Email))
	if claims.Email == "" {
		return MintResult{}, ErrNotAuthorized
	}
	all, err := i.readIssuances(ctx)
	if err != nil {
		return MintResult{}, err
	}
	var found *Issuance
	for j := range all.Entries {
		if all.Entries[j].Label == label {
			found = &all.Entries[j]
			break
		}
	}
	if found == nil {
		return MintResult{}, ErrNotFound
	}
	// EqualFold rather than == so admin-tooled or pre-canonicalization
	// issuance rows still match a canonical caller email. Forward
	// traffic from Mint always lands with canonical iss.Email so the
	// two are equivalent; this is the defense-in-depth shape for
	// non-canonical legacy entries, matching Revoke's owner check.
	if !strings.EqualFold(found.Email, claims.Email) {
		return MintResult{}, ErrNotOwner
	}
	world := i.lookupWorld(found.World)
	if world == nil {
		return MintResult{}, fmt.Errorf("world %q not configured for label %q", found.World, label)
	}
	if !worldAllows(&world.Allow, claims) {
		return MintResult{}, ErrNotAuthorized
	}
	now := i.clock()
	minted, err := i.mintForWorld(ctx, world, claims, now, found.Paths, found.Operations)
	if err != nil {
		return MintResult{}, fmt.Errorf("mint replacement for %s: %w", label, err)
	}
	// Snapshot of the old label before mutation; revokeIssuance
	// removes by iss.Label so passing found directly is safe, but
	// reading it back from `all.Entries` post-mint would race with
	// a fresh-mint that re-added it (impossible in practice since
	// labels are opaque random IDs, but the snapshot makes the
	// data-flow obvious).
	oldLabel := found.Label
	if err := i.revokeIssuance(ctx, found); err != nil {
		// Partial success: the new token is live and recorded.
		// The old label stays both in the world Secret and the
		// issuances Secret; sweep will retire it on expiry (or
		// drift, if the operator hand-edits the world Secret in
		// the meantime). Return the new mint plus the wrapped
		// error so the HTTP layer can log-warn-and-200.
		return minted, fmt.Errorf("revoke old label %s: %w", oldLabel, err)
	}
	return minted, nil
}

// revokeIssuance executes the two-Secret mutation that retires a token:
// remove from the world's tokens Secret first so the token stops working
// immediately, then remove from the broker's issuances Secret. A
// transient failure between the two writes leaves an orphan in
// issuances; the expiry sweeper's drift-pruning catches those next tick
// by comparing both Secrets.
//
// Shared between Revoke (after the owner check) and the sweeper (which
// already holds the entry from its iteration). Surfaces the
// world-not-configured case as an explicit error so silently dropping
// the issuance can't leave a still-valid token in an orphaned world.
func (i *Issuer) revokeIssuance(ctx context.Context, iss *Issuance) error {
	world := i.lookupWorld(iss.World)
	if world == nil {
		return fmt.Errorf("world %q not configured for label %q", iss.World, iss.Label)
	}
	if err := i.removeFromWorldSecret(ctx, world, iss.Label); err != nil {
		return fmt.Errorf("remove from world %s: %w", world.Name, err)
	}
	return i.removeIssuance(ctx, iss.Label)
}

func (i *Issuer) lookupWorld(name string) *WorldConfig {
	for j := range i.cfg.Worlds {
		if i.cfg.Worlds[j].Name == name {
			return &i.cfg.Worlds[j]
		}
	}
	return nil
}

func (i *Issuer) appendToWorldSecret(ctx context.Context, w *WorldConfig, label string, entry *token.Entry) error {
	return i.mutateSecret(ctx, w.Namespace, w.TokensSecret, TokensSecretKey, func(existing []byte) ([]byte, error) {
		return token.AppendBytes(existing, label, entry)
	})
}

func (i *Issuer) removeFromWorldSecret(ctx context.Context, w *WorldConfig, label string) error {
	return i.mutateSecret(ctx, w.Namespace, w.TokensSecret, TokensSecretKey, func(existing []byte) ([]byte, error) {
		return token.RemoveBytes(existing, label)
	})
}

func (i *Issuer) appendIssuance(ctx context.Context, iss *Issuance) error {
	return i.mutateSecret(ctx, i.cfg.Server.BrokerNamespace, i.cfg.Server.IssuancesSecret, IssuancesSecretKey, func(existing []byte) ([]byte, error) {
		var current Issuances
		if len(existing) > 0 {
			if err := json.Unmarshal(existing, &current); err != nil {
				return nil, fmt.Errorf("decode issuances: %w", err)
			}
		}
		// Labels must be globally unique across all worlds.
		// removeIssuance filters by label across the whole Secret, so a
		// cross-world collision would orphan one world's token after
		// the first revoke. Reject the duplicate and let the caller
		// retry with a fresh label.
		for j := range current.Entries {
			if current.Entries[j].Label == iss.Label {
				return nil, token.ErrLabelExists
			}
		}
		current.Entries = append(current.Entries, *iss)
		return json.Marshal(current)
	})
}

func (i *Issuer) removeIssuance(ctx context.Context, label string) error {
	return i.mutateSecret(ctx, i.cfg.Server.BrokerNamespace, i.cfg.Server.IssuancesSecret, IssuancesSecretKey, func(existing []byte) ([]byte, error) {
		if len(existing) == 0 {
			return existing, nil
		}
		var current Issuances
		if err := json.Unmarshal(existing, &current); err != nil {
			return nil, fmt.Errorf("decode issuances: %w", err)
		}
		filtered := current.Entries[:0]
		for j := range current.Entries {
			if current.Entries[j].Label != label {
				filtered = append(filtered, current.Entries[j])
			}
		}
		current.Entries = filtered
		return json.Marshal(current)
	})
}

func (i *Issuer) readIssuances(ctx context.Context) (Issuances, error) {
	secret, err := i.k8s.CoreV1().Secrets(i.cfg.Server.BrokerNamespace).Get(ctx, i.cfg.Server.IssuancesSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Issuances{}, nil
	}
	if err != nil {
		return Issuances{}, fmt.Errorf("read issuances secret: %w", err)
	}
	raw := secret.Data[IssuancesSecretKey]
	if len(raw) == 0 {
		return Issuances{}, nil
	}
	var iss Issuances
	if err := json.Unmarshal(raw, &iss); err != nil {
		return Issuances{}, fmt.Errorf("decode issuances: %w", err)
	}
	return iss, nil
}

// mutateSecret performs an optimistic-concurrency read-modify-write on
// the Secret data[key]. If the Secret does not exist, it is created with
// the named key set to the mutate-result on empty input. Retries
// resourceVersion conflicts up to maxConflictRetries; surfaces all other
// errors immediately.
func (i *Issuer) mutateSecret(ctx context.Context, namespace, name, key string, mutate func([]byte) ([]byte, error)) error {
	for range maxConflictRetries {
		secret, getErr := i.k8s.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
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
			fresh := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{key: next},
			}
			_, createErr := i.k8s.CoreV1().Secrets(namespace).Create(ctx, fresh, metav1.CreateOptions{})
			if createErr == nil {
				return nil
			}
			if apierrors.IsAlreadyExists(createErr) {
				continue
			}
			return fmt.Errorf("create secret %s/%s: %w", namespace, name, createErr)
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte, 1)
		}
		secret.Data[key] = next
		_, updateErr := i.k8s.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{})
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
