package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/knowledgeserver/worldsfragment"
	"golang.org/x/sync/singleflight"
)

// Dynamic tenant provisioning (memory-broker plan Phase 3). One
// registry Secret is the source of truth; broker world set and server
// fragment are projections, so a crash between steps converges on retry.

// registrySyncInterval paces the background registry poll that keeps
// every replica's dynamic world set converged with provisioning done
// elsewhere.
const registrySyncInterval = 10 * time.Second

// ErrProvisioningDenied is returned when the provisioning gate rejects
// the identity (mode static, allowlist miss, or capacity).
var ErrProvisioningDenied = errors.New("broker: provisioning denied")

// ErrTenantCapacity is returned when maxTenants is reached.
var ErrTenantCapacity = errors.New("broker: tenant capacity reached")

// ErrTenantDeprovisioning is returned when the slug is tombstoned: a
// deprovision is (or crashed) mid-cleanup and must finish first.
var ErrTenantDeprovisioning = errors.New("broker: tenant is being deprovisioned")

// BucketCreator ensures tenant buckets exist AND carry the locked-down
// tenant posture; implementations must refuse to adopt a pre-existing
// bucket with looser access. Production wraps GCS; tests fake it.
type BucketCreator interface {
	EnsureBucket(ctx context.Context, bucket string) error
}

// BucketDeleter is the optional destructive sibling used only by the
// explicit deprovision flow.
type BucketDeleter interface {
	DeleteBucket(ctx context.Context, bucket string) error
}

// WorldsApplier is a knowledge server in this process that takes the
// rendered worlds fragment directly, ahead of the mounted Secret.
type WorldsApplier interface {
	ApplyWorldsFragment(fragment []byte) error
}

// tenantRecord is one provisioned tenant in the registry.
type tenantRecord struct {
	Email   string `json:"email"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	WorldID string `json:"worldID"`
	Created string `json:"created"`
	// Deleting tombstones a tenant mid-deprovision: provisioning the
	// slug is blocked and every projection drops the world until the
	// cleanup finishes and removes the record.
	Deleting bool `json:"deleting,omitempty"`
}

// tenantRegistry is the registry document shape.
type tenantRegistry struct {
	Tenants map[string]tenantRecord `json:"tenants"`
}

// Provisioner creates tenant worlds on first arrival. Run wires one into
// the gateway when provisioning is enabled; static mode has none.
type Provisioner struct {
	cfg      *core.Config
	registry *core.WorldRegistry
	store    core.SecretStore
	buckets  BucketCreator
	worlds   WorldsApplier
	log      *slog.Logger
	clock    func() time.Time

	// refusals answers a refused identity from memory, off every lock.
	refusals tenantRefusals
	// flights single-flights one identity's provisioning. Identities never
	// wait on each other: the registry Secret's optimistic concurrency keeps
	// their records apart, and a slow bucket is its own tenant's alone.
	flights singleflight.Group
	// projectMu makes "read the registry, write the fragment, apply it" one
	// step: both must follow registry read order, and the Secret store gives
	// no version to order by. It spans Secret round trips only.
	projectMu sync.Mutex

	// mu guards the in-memory state below; no I/O runs under it.
	mu sync.Mutex
	// lastSync is the registry payload hash of the last applied sync,
	// so the 10s poll skips rebuilding an unchanged world set.
	lastSync [32]byte
	// rendered numbers the fragments in the order projectMu rendered them.
	rendered uint64
	// applied counts the registries applied, so a refusal decided on an
	// older one is not remembered past a newer.
	applied uint64

	// pushMu orders the in-process pushes, which run outside every other
	// lock because the server opens worlds under them; pushed is the newest
	// one sent.
	pushMu sync.Mutex
	pushed uint64
}

// provisionTimeout bounds one identity's flight once its callers may have
// left; every step is idempotent, so a cut one converges on the next call.
const provisionTimeout = 2 * time.Minute

// ProvisionerDeps is what a Provisioner needs.
type ProvisionerDeps struct {
	Store   core.SecretStore
	Buckets BucketCreator
	// Registry is the live world set provisioning publishes into; nil builds
	// a private one, which is all the deprovision command needs.
	Registry *core.WorldRegistry
	// Worlds is the in-process server; nil when the worlds run elsewhere.
	Worlds WorldsApplier
	Log    *slog.Logger
	Clock  func() time.Time
}

// NewProvisioner builds a Provisioner; nil Log and Clock take defaults.
func NewProvisioner(cfg *core.Config, deps ProvisionerDeps) *Provisioner {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Registry == nil {
		deps.Registry = core.NewWorldRegistry(cfg.Worlds)
	}
	return &Provisioner{cfg: cfg, registry: deps.Registry, store: deps.Store, buckets: deps.Buckets, worlds: deps.Worlds, log: deps.Log, clock: deps.Clock}
}

// DeprovisionTenant removes slug: its registry entry, with it the fragment
// entry every writer projects from the registry, and optionally the bucket.
// Idempotent; operator-invoked only.
func (p *Provisioner) DeprovisionTenant(ctx context.Context, slug string, deleteBucket bool) (found bool, err error) {
	// Resolve the deleter BEFORE any destructive round trip: failing
	// after the registry rewrite would leave a half-removed tenant.
	var deleter BucketDeleter
	if deleteBucket {
		var ok bool
		if deleter, ok = p.buckets.(BucketDeleter); !ok {
			return false, errors.New("broker: bucket deletion requested but the bucket backend cannot delete")
		}
	}
	// Phase 1: tombstone the record. Provisioning the slug is blocked
	// and every projection drops the world; a crash mid-cleanup leaves
	// the tombstone, so a rerun converges and re-provision never races.
	found, err = p.setTombstone(ctx, slug, true)
	if err != nil {
		return false, err
	}
	// The fragment follows the registry: the tombstoned slug leaves it,
	// tenants other pods provisioned meanwhile stay.
	if err := p.project(ctx); err != nil {
		return found, fmt.Errorf("rewrite worlds fragment: %w", err)
	}
	if deleter != nil {
		if err := deleter.DeleteBucket(ctx, p.bucketName(slug)); err != nil {
			return found, err
		}
	}
	// Phase 2: cleanup done; clear the tombstone so the slug is free.
	if _, err := p.setTombstone(ctx, slug, false); err != nil {
		return found, fmt.Errorf("clear tombstone for %q: %w", slug, err)
	}
	if err := p.project(ctx); err != nil {
		return found, fmt.Errorf("rewrite worlds fragment: %w", err)
	}
	if found {
		p.log.Info("tenant deprovisioned", "world", slug, "bucketDeleted", deleteBucket)
	}
	return found, nil
}

// setTombstone marks slug's record deleting, or with tombstone false removes
// it, and reports whether the registry held it.
func (p *Provisioner) setTombstone(ctx context.Context, slug string, tombstone bool) (found bool, err error) {
	err = p.store.Mutate(ctx, core.RegistryRef(p.cfg), func(existing []byte) ([]byte, error) {
		registry, decodeErr := decodeRegistry(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		record, ok := registry.Tenants[slug]
		found = ok
		switch {
		case !tombstone:
			delete(registry.Tenants, slug)
		case ok:
			record.Deleting = true
			registry.Tenants[slug] = record
		}
		encoded, encErr := json.Marshal(&registry)
		if encErr != nil {
			return nil, fmt.Errorf("encode tenant registry: %w", encErr)
		}
		return encoded, nil
	})
	return found, err
}

// project brings the fragment, this pod's world set and the server in this
// process in line with the registry as it stands now.
func (p *Provisioner) project(ctx context.Context) error {
	fragment, err := p.withLock(func() (renderedFragment, error) { return p.projectLocked(ctx) })
	if err != nil {
		return err
	}
	p.pushWorlds(fragment)
	return nil
}

// withLock runs render under projectMu and lets go before the caller pushes
// what it rendered: the server opens worlds under a push.
func (p *Provisioner) withLock(render func() (renderedFragment, error)) (renderedFragment, error) {
	p.projectMu.Lock()
	defer p.projectMu.Unlock()
	return render()
}

// projectLocked writes the fragment and applies the registry it was rendered
// from; the caller holds projectMu and pushes the fragment once it has let go.
func (p *Provisioner) projectLocked(ctx context.Context) (renderedFragment, error) {
	written, err := p.writeWorldsFragment(ctx)
	if err != nil {
		return renderedFragment{}, err
	}
	p.applySnapshot(&written.registry)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rendered++
	return renderedFragment{body: written.fragment, seq: p.rendered, payload: written.payload}, nil
}

// renderedFragment is a fragment and its place in the order projectMu rendered
// them, so a push that lost a race never puts an older world set in force.
type renderedFragment struct {
	body []byte
	seq  uint64
	// payload is the hash of the registry the fragment projects.
	payload [32]byte
}

// applySnapshot publishes a registry snapshot to this pod's world set and
// identity index. The registry changed, so every remembered refusal may now
// be admitted and is forgotten.
func (p *Provisioner) applySnapshot(registry *tenantRegistry) {
	// One step with the count: refuse remembers only against the latest.
	p.mu.Lock()
	p.refusals.clear()
	p.applied++
	p.mu.Unlock()
	rejected := p.registry.SetDynamic(p.worldsFromRegistry(registry), tenantIndexFromRegistry(registry))
	if len(rejected) > 0 {
		// Written by hand or by an older build: no tool URL can name them.
		p.log.Error("tenant registry holds world names that are not DNS labels; not served", "worlds", rejected)
	}
}

// tenantIndexFromRegistry projects live records to identityKey -> slug.
// First live slug in sorted order wins (matches findTenantByIdentity);
// tombstoned records stay out so a deprovision denies via the slow path.
func tenantIndexFromRegistry(registry *tenantRegistry) map[string]string {
	index := make(map[string]string, len(registry.Tenants))
	for _, slug := range sortedSlugs(registry) {
		record := registry.Tenants[slug]
		if record.Deleting {
			continue
		}
		key := core.IdentityKey(record.Issuer, record.Subject)
		if _, ok := index[key]; !ok {
			index[key] = slug
		}
	}
	return index
}

// findTenantByIdentity resolves a registry record by stable identity;
// the slug is the world name pinned at provision time. First sorted
// live slug wins (matches the index); tombstoned only when none live.
func findTenantByIdentity(registry *tenantRegistry, issuer, subject string) (string, tenantRecord, bool) {
	var tombSlug string
	var tombRecord tenantRecord
	tombFound := false
	for _, slug := range sortedSlugs(registry) {
		record := registry.Tenants[slug]
		if record.Issuer != issuer || record.Subject != subject {
			continue
		}
		if record.Deleting {
			if !tombFound {
				tombSlug, tombRecord, tombFound = slug, record, true
			}
			continue
		}
		return slug, record, true
	}
	return tombSlug, tombRecord, tombFound
}

// slugSuffixBytes are the identity hash lengths a slug may end in: the
// short one reads well, the long one settles a collision with another tenant.
var slugSuffixBytes = []int{4, 8}

// TenantSlug derives the world name: sanitized email local part plus a short
// identity hash, stable because the suffix depends on issuer|sub only. Two
// identities can share one: freeSlug never hands out a slug another holds.
func TenantSlug(issuer, subject, email string) string {
	return tenantSlug(issuer, subject, email, slugSuffixBytes[0])
}

func tenantSlug(issuer, subject, email string, suffixBytes int) string {
	local, _, _ := strings.Cut(email, "@")
	local = strings.ToLower(local)
	var b strings.Builder
	lastHyphen := true // suppress leading hyphens
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	cleaned := strings.TrimRight(b.String(), "-")
	if len(cleaned) > 20 {
		cleaned = strings.TrimRight(cleaned[:20], "-")
	}
	if cleaned == "" {
		cleaned = "soul"
	}
	sum := sha256.Sum256([]byte(core.IdentityKey(issuer, subject)))
	return cleaned + "-" + hex.EncodeToString(sum[:suffixBytes])
}

// ErrSlugTaken means every slug the identity could take is another tenant's.
var ErrSlugTaken = errors.New("broker: tenant slug is held by another identity")

// freeSlug is the identity's slug, widened past a record another identity
// holds: a slug is a world and a bucket, never to be overwritten.
func freeSlug(registry *tenantRegistry, issuer, subject, email string) (string, error) {
	for _, n := range slugSuffixBytes {
		slug := tenantSlug(issuer, subject, email, n)
		if _, taken := registry.Tenants[slug]; !taken {
			return slug, nil
		}
	}
	return "", ErrSlugTaken
}

// tenantWorldID derives a deterministic canonical UUID (version 4
// shape, RFC 4122 variant) from the identity, so concurrent
// provisioners mint the same worldID without coordination.
func tenantWorldID(issuer, subject string) string {
	sum := sha256.Sum256([]byte("worldid|" + core.IdentityKey(issuer, subject)))
	b := sum[:16]
	b[6] = 0x40 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (p *Provisioner) bucketName(slug string) string {
	return strings.TrimPrefix(p.cfg.Provisioning.BucketPrefix, "gs://") + slug
}

// admits applies the provisioning gate to a new identity. Existing
// tenants (already in the registry) bypass it so tightening the
// allowlist later never locks out provisioned users.
func (p *Provisioner) admits(claims *core.Claims) error {
	switch p.cfg.Provisioning.Mode {
	case core.ProvisionOpen:
		return nil
	case core.ProvisionAllowlisted:
		if core.WorldAllows(&p.cfg.Provisioning.Allow, claims) {
			return nil
		}
		return ErrProvisioningDenied
	default:
		return ErrProvisioningDenied
	}
}

// EnsureTenant provisions or converges the caller's world and returns its
// broker-side WorldConfig. Every step is idempotent and keyed by the pinned
// slug. One identity's calls share a flight; other identities run beside it.
func (p *Provisioner) EnsureTenant(ctx context.Context, claims *core.Claims) (core.WorldConfig, error) {
	identity := core.IdentityKey(p.cfg.OIDC.Issuer, claims.Subject)
	if refusal := p.refusals.recent(identity, p.clock()); refusal != nil {
		return core.WorldConfig{}, refusal
	}
	flight := p.flights.DoChan(identity, func() (any, error) {
		// The flight outlives the caller that started it: one leaving must
		// not fail the others sharing it.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionTimeout)
		defer cancel()
		return p.provision(flightCtx, identity, claims)
	})
	select {
	case <-ctx.Done():
		return core.WorldConfig{}, ctx.Err()
	case result := <-flight:
		if result.Err != nil {
			return core.WorldConfig{}, result.Err
		}
		return result.Val.(core.WorldConfig), nil
	}
}

// provision is one identity's flight: its record and bucket under no lock,
// then the projection every identity takes in turn, then the push.
func (p *Provisioner) provision(ctx context.Context, identity string, claims *core.Claims) (core.WorldConfig, error) {
	p.mu.Lock()
	applied := p.applied
	p.mu.Unlock()
	world, err := p.ensureTenant(ctx, claims)
	if err != nil {
		p.refuse(identity, claims.Subject, refusal{err: err, applied: applied})
		return core.WorldConfig{}, err
	}
	// The fragment, then this pod's world set, both from the registry as it
	// stands now; other replicas converge via the sync loop.
	fragment, err := p.withLock(func() (renderedFragment, error) { return p.projectLocked(ctx) })
	if err != nil {
		return core.WorldConfig{}, fmt.Errorf("write worlds fragment: %w", err)
	}
	// A slow open must not hold up any other identity's provisioning.
	p.pushWorlds(fragment)
	return world, nil
}

// refusal is why an identity was not provisioned and how many registries
// this pod had applied when the decision's flight began.
type refusal struct {
	err     error
	applied uint64
}

// refuse logs a refusal and remembers gate and capacity ones until
// refusalTTL or a registry change. One decided before a newer registry is
// not kept, nor a deprovisioning one: either can clear before this pod syncs.
func (p *Provisioner) refuse(identity, subject string, refused refusal) {
	switch {
	case errors.Is(refused.err, ErrProvisioningDenied):
		p.log.Warn("tenant provisioning denied by gate", "subject", core.HashSubject(subject))
	case errors.Is(refused.err, ErrTenantCapacity):
		p.log.Warn("tenant provisioning denied at capacity", "subject", core.HashSubject(subject))
	case errors.Is(refused.err, ErrTenantDeprovisioning):
		p.log.Warn("tool call denied for tombstoned tenant", "subject", core.HashSubject(subject))
		return
	default:
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.applied == refused.applied {
		p.refusals.remember(identity, refused.err, p.clock())
	}
}

// ensureTenant converges the identity's registry record and bucket and
// returns its world. It holds no lock: the registry write is the Secret's
// optimistic read-modify-write, and the bucket is the identity's own.
func (p *Provisioner) ensureTenant(ctx context.Context, claims *core.Claims) (core.WorldConfig, error) {

	email := core.CanonicalEmail(claims.Email)
	issuer := p.cfg.OIDC.Issuer

	// Step 1: registry entry, under the Secret's optimistic concurrency.
	// Lookup is by identity, never the recomputed slug: an email change
	// refreshes the record instead of orphaning the world.
	var slug string
	created := false
	err := p.store.Mutate(ctx, core.RegistryRef(p.cfg), func(existing []byte) ([]byte, error) {
		registry, decodeErr := decodeRegistry(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		created = false
		if found, record, ok := findTenantByIdentity(&registry, issuer, claims.Subject); ok {
			if record.Deleting {
				return nil, ErrTenantDeprovisioning
			}
			slug = found
			if record.Email != email {
				record.Email = email
				registry.Tenants[slug] = record
			}
		} else {
			if gateErr := p.admits(claims); gateErr != nil {
				return nil, gateErr
			}
			maxTenants := p.cfg.Provisioning.MaxTenants
			if maxTenants > 0 && len(registry.Tenants) >= maxTenants {
				return nil, ErrTenantCapacity
			}
			// The identity lookup above missed, so a record under its slug is
			// another tenant's.
			var slugErr error
			if slug, slugErr = freeSlug(&registry, issuer, claims.Subject, email); slugErr != nil {
				return nil, slugErr
			}
			registry.Tenants[slug] = tenantRecord{
				Email:   email,
				Issuer:  issuer,
				Subject: claims.Subject,
				WorldID: tenantWorldID(issuer, claims.Subject),
				Created: p.clock().UTC().Format(time.RFC3339),
			}
			created = true
		}
		encoded, encErr := json.Marshal(&registry)
		if encErr != nil {
			return nil, fmt.Errorf("encode tenant registry: %w", encErr)
		}
		return encoded, nil
	})
	if err != nil {
		return core.WorldConfig{}, err
	}

	// No already-provisioned fast path here: the registry sync can
	// publish a world whose bucket/fragment ensure crashed mid-way, so
	// every EnsureTenant re-runs the idempotent steps to converge.

	// Step 2: backend storage. EnsureBucket is idempotent.
	if err := p.buckets.EnsureBucket(ctx, p.bucketName(slug)); err != nil {
		return core.WorldConfig{}, fmt.Errorf("ensure bucket for %q: %w", slug, err)
	}
	if created {
		p.log.Info("tenant provisioned", "world", slug, "subject", core.HashSubject(claims.Subject))
	}
	return p.cfg.Provisioning.TenantWorld(slug, email), nil
}

func decodeRegistry(existing []byte) (tenantRegistry, error) {
	registry := tenantRegistry{Tenants: map[string]tenantRecord{}}
	if len(existing) == 0 {
		return registry, nil
	}
	if err := json.Unmarshal(existing, &registry); err != nil {
		return tenantRegistry{}, fmt.Errorf("decode tenant registry: %w", err)
	}
	if registry.Tenants == nil {
		registry.Tenants = map[string]tenantRecord{}
	}
	return registry, nil
}

// renderWorldsFragment is the fragment of registry: every live tenant, none
// that is tombstoned. A pure projection, so any writer repairs any other's.
func (p *Provisioner) renderWorldsFragment(registry *tenantRegistry) ([]byte, error) {
	worlds := make([]worldsfragment.World, 0, len(registry.Tenants))
	for slug, record := range registry.Tenants {
		if record.Deleting {
			continue
		}
		world := worldsfragment.World{
			Name:        slug,
			Authorities: []string{slug + "." + p.cfg.Provisioning.AuthorityDomain},
			Bucket:      worldsfragment.Bucket{URL: "gs://" + p.bucketName(slug), WorldID: record.WorldID},
			Policy:      &worldsfragment.Policy{Governance: "open"},
			Gated:       true,
			// The server seeds the tenant policy; the broker writes none.
			Seed: worldsfragment.SeedMemory,
		}
		if quota := p.cfg.Provisioning.MaxDocumentsPerTenant; quota > 0 {
			world.Limits = &worldsfragment.Limits{MaxDocuments: quota}
		}
		worlds = append(worlds, world)
	}
	return worldsfragment.Render(worlds)
}

// writeWorldsFragment stores the fragment of the registry as it stands at
// the write, read again on every attempt: a snapshot taken earlier could
// drop a tenant provisioned since, or bring back one deprovisioned since.
func (p *Provisioner) writeWorldsFragment(ctx context.Context) (projection, error) {
	var written projection
	err := p.store.Mutate(ctx, core.WorldsFragmentRef(p.cfg), func([]byte) ([]byte, error) {
		raw, err := core.ReadSecret(ctx, p.store, core.RegistryRef(p.cfg))
		if err != nil {
			return nil, err
		}
		registry, err := decodeRegistry(raw)
		if err != nil {
			return nil, err
		}
		fragment, err := p.renderWorldsFragment(&registry)
		written = projection{fragment: fragment, registry: registry, payload: sha256.Sum256(raw)}
		return fragment, err
	})
	return written, err
}

// projection is a stored fragment, the registry it was rendered from and
// that registry's payload hash.
type projection struct {
	fragment []byte
	registry tenantRegistry
	payload  [32]byte
}

// pushWorlds hands a fragment to the server in this process, ahead of the
// mount, unless a newer one went first, and reports whether the server holds
// it or a newer one. A failed push is logged: the mount still converges.
func (p *Provisioner) pushWorlds(fragment renderedFragment) bool {
	if p.worlds == nil {
		return true
	}
	p.pushMu.Lock()
	defer p.pushMu.Unlock()
	if fragment.seq <= p.pushed {
		return true
	}
	if err := p.worlds.ApplyWorldsFragment(fragment.body); err != nil {
		p.log.Warn("in-process worlds push failed; the mounted fragment will catch up", "err", err)
		return false
	}
	p.pushed = fragment.seq
	return true
}

// worldsFromRegistry renders the broker-side dynamic world set.
func (p *Provisioner) worldsFromRegistry(registry *tenantRegistry) []core.WorldConfig {
	slugs := sortedSlugs(registry)
	worlds := make([]core.WorldConfig, 0, len(slugs))
	for _, slug := range slugs {
		if registry.Tenants[slug].Deleting {
			continue // tombstoned: out of service until cleanup finishes
		}
		world := p.cfg.Provisioning.TenantWorld(slug, registry.Tenants[slug].Email)
		world.Generation = registry.Tenants[slug].Created
		worlds = append(worlds, world)
	}
	return worlds
}

// sortedSlugs is the one ordering rule for registry projections (the
// fragment golden fixture depends on it).
func sortedSlugs(registry *tenantRegistry) []string {
	return slices.Sorted(maps.Keys(registry.Tenants))
}

// SyncRegistry republishes the dynamic world set and the fragment when the
// registry changed since the last sync that took. The registry is read and
// applied under projectMu, so a stale read never overwrites a local mutation.
func (p *Provisioner) SyncRegistry(ctx context.Context) error {
	changed := false
	fragment, err := p.withLock(func() (renderedFragment, error) {
		existing, err := core.ReadSecret(ctx, p.store, core.RegistryRef(p.cfg))
		if err != nil || sha256.Sum256(existing) == p.synced() {
			return renderedFragment{}, err
		}
		changed = true
		// The fragment is a projection of the registry, so writing it again
		// repairs whatever another writer left behind.
		fragment, err := p.projectLocked(ctx)
		if err != nil {
			// lastSync stays behind so the next tick retries the converge.
			return renderedFragment{}, fmt.Errorf("converge worlds fragment: %w", err)
		}
		return fragment, nil
	})
	if err != nil || !changed {
		return err
	}
	// A refused push leaves lastSync behind so the next tick pushes again;
	// the Secret projection converges the world meanwhile.
	if p.pushWorlds(fragment) {
		p.mu.Lock()
		p.lastSync = fragment.payload
		p.mu.Unlock()
	}
	return nil
}

// synced is the registry payload hash of the last sync that took.
func (p *Provisioner) synced() [32]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSync
}

// RunRegistrySync polls the registry so every replica converges on
// tenants provisioned elsewhere. Blocks until ctx ends.
func (p *Provisioner) RunRegistrySync(ctx context.Context) {
	ticker := time.NewTicker(registrySyncInterval)
	defer ticker.Stop()
	if err := p.SyncRegistry(ctx); err != nil {
		p.log.Warn("tenant registry sync failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.SyncRegistry(ctx); err != nil {
				p.log.Warn("tenant registry sync failed", "err", err)
			}
		}
	}
}
