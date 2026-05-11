package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	worlds := i.authorizedWorlds(claims.Email)
	if len(worlds) == 0 {
		return nil, ErrNotAuthorized
	}
	now := i.clock()
	results := make([]MintResult, 0, len(worlds))
	for _, w := range worlds {
		r, err := i.mintForWorld(ctx, w, claims, now)
		if err != nil {
			return results, fmt.Errorf("mint for world %s: %w", w.Name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func (i *Issuer) authorizedWorlds(email string) []*WorldConfig {
	out := make([]*WorldConfig, 0, len(i.cfg.Worlds))
	for j := range i.cfg.Worlds {
		w := &i.cfg.Worlds[j]
		if domainMatches(email, w.AllowDomains) {
			out = append(out, w)
		}
	}
	return out
}

func domainMatches(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range allowed {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

func (i *Issuer) mintForWorld(ctx context.Context, w *WorldConfig, claims Claims, now time.Time) (MintResult, error) {
	for attempt := range maxLabelRetries {
		label, err := i.labelGen()
		if err != nil {
			return MintResult{}, err
		}
		minted, err := token.Generate(label, w.DefaultToken.Paths, w.DefaultToken.Operations)
		if err != nil {
			return MintResult{}, fmt.Errorf("generate token: %w", err)
		}
		expiresAt := now.Add(w.DefaultToken.ExpiresAfter)
		minted.Entry.Expires = expiresAt.UTC().Format(time.RFC3339)

		err = i.appendToWorldSecret(ctx, w, label, &minted.Entry)
		if errors.Is(err, token.ErrLabelExists) {
			if attempt == maxLabelRetries-1 {
				return MintResult{}, fmt.Errorf("%d consecutive label collisions", maxLabelRetries)
			}
			continue
		}
		if err != nil {
			return MintResult{}, err
		}
		issuance := Issuance{
			Label:      label,
			Email:      claims.Email,
			World:      w.Name,
			Paths:      append([]string(nil), w.DefaultToken.Paths...),
			Operations: append([]string(nil), w.DefaultToken.Operations...),
			IssuedAt:   now,
			ExpiresAt:  expiresAt,
		}
		if err := i.appendIssuance(ctx, &issuance); err != nil {
			// World secret already activated the token; without an
			// issuance record the broker can't revoke or sweep it.
			// Best-effort rollback so we don't leave an active
			// untracked token. If rollback also fails, surface both.
			if rbErr := i.removeFromWorldSecret(ctx, w, label); rbErr != nil {
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
// issuance.
//
// World Secret is removed first so the token stops working immediately;
// the issuance entry is the second write, which means a transient failure
// between the two writes leaves an orphan in issuances. The Slice D
// sweeper prunes such orphans by comparing both Secrets every tick.
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
	world := i.lookupWorld(found.World)
	if world == nil {
		// The issuance points at a world the broker is no longer
		// configured for. Silently dropping the issuance entry would
		// report success while leaving a still-valid token in that
		// world's tokens Secret. Surface the misconfiguration instead.
		return fmt.Errorf("world %q not configured for label %q", found.World, label)
	}
	if err := i.removeFromWorldSecret(ctx, world, label); err != nil {
		return fmt.Errorf("remove from world %s: %w", world.Name, err)
	}
	return i.removeIssuance(ctx, label)
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
