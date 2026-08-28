package broker

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
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Dynamic tenant provisioning (memory-broker plan Phase 3). One
// registry Secret is the source of truth; broker world set and server
// fragment are projections, so a crash between steps converges on retry.

// registrySecretKey is the data key holding the tenant registry JSON.
const registrySecretKey = "registry.json"

// worldsFragmentKey is the data key holding the knowledge-server worlds
// fragment the server mounts and hot-reloads.
const worldsFragmentKey = "worlds.yaml"

// registrySyncInterval paces the background registry poll that keeps
// every replica's dynamic world set converged with provisioning done
// elsewhere.
const registrySyncInterval = 10 * time.Second

// ErrProvisioningDenied is returned when the provisioning gate rejects
// the identity (mode static, allowlist miss, or capacity).
var ErrProvisioningDenied = errors.New("broker: provisioning denied")

// ErrTenantCapacity is returned when maxTenants is reached.
var ErrTenantCapacity = errors.New("broker: tenant capacity reached")

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

// tenantRecord is one provisioned tenant in the registry.
type tenantRecord struct {
	Email   string `json:"email"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	WorldID string `json:"worldID"`
	Created string `json:"created"`
}

// tenantRegistry is the registry document shape.
type tenantRegistry struct {
	Tenants map[string]tenantRecord `json:"tenants"`
}

// Provisioner creates tenant worlds on first arrival. Owned by Server
// (EnableProvisioning); nil on the knowledge broker and on memory
// brokers running static mode.
type Provisioner struct {
	cfg     *Config
	store   SecretStore
	buckets BucketCreator
	log     *slog.Logger
	clock   func() time.Time

	// mu single-flights provisioning within one pod; cross-pod
	// convergence rides the registry Secret's optimistic concurrency.
	mu sync.Mutex
	// lastSync is the registry payload hash of the last applied sync,
	// so the 10s poll skips rebuilding an unchanged world set.
	lastSync [32]byte
}

// newProvisioner builds a standalone Provisioner (RunDeprovision, or
// any flow without a Server).
func newProvisioner(cfg *Config, store SecretStore, buckets BucketCreator, log *slog.Logger) *Provisioner {
	if log == nil {
		log = slog.Default()
	}
	return &Provisioner{cfg: cfg, store: store, buckets: buckets, log: log, clock: time.Now}
}

// EnableProvisioning wires a Provisioner onto the Server. Called by the
// memory-broker binary when cfg.Provisioning is enabled.
func (s *Server) EnableProvisioning(buckets BucketCreator) *Provisioner {
	p := newProvisioner(s.cfg, s.worldWriteTokens.store, buckets, s.log)
	p.clock = s.clock
	s.provisioner = p
	return p
}

// DeprovisionTenant removes slug: registry entry, authoritative fragment
// rewrite (the ONE flow allowed to drop worlds), tokens key, write-token
// record, and optionally the bucket. Idempotent; operator-invoked only.
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
	p.mu.Lock()
	defer p.mu.Unlock()

	var snapshot tenantRegistry
	err = p.store.Mutate(ctx, p.cfg.registryRef(), func(existing []byte) ([]byte, error) {
		registry, decodeErr := decodeRegistry(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		_, found = registry.Tenants[slug]
		delete(registry.Tenants, slug)
		snapshot = registry
		encoded, encErr := json.Marshal(&registry)
		if encErr != nil {
			return nil, fmt.Errorf("encode tenant registry: %w", encErr)
		}
		return encoded, nil
	})
	if err != nil {
		return false, err
	}

	// Fragment: authoritative rewrite (no union) so the world actually
	// leaves the knowledge server. A racing EnsureTenant union from a
	// pre-removal snapshot can resurrect the entry; rerun to converge.
	if err := p.store.Mutate(ctx, p.cfg.worldsFragmentRef(), func([]byte) ([]byte, error) {
		return p.renderWorldsFragmentAuthoritative(&snapshot)
	}); err != nil {
		return found, fmt.Errorf("rewrite worlds fragment: %w", err)
	}

	world := p.cfg.Provisioning.tenantWorld(slug, "")
	if err := p.store.Delete(ctx, p.cfg.worldTokensRef(&world)); err != nil {
		return found, fmt.Errorf("delete tokens key for %q: %w", slug, err)
	}
	if err := p.store.Delete(ctx, p.cfg.worldWriteTokenRef(slug)); err != nil {
		return found, fmt.Errorf("delete write-token record for %q: %w", slug, err)
	}

	if deleter != nil {
		if err := deleter.DeleteBucket(ctx, p.bucketName(slug)); err != nil {
			return found, err
		}
	}

	p.applySnapshot(&snapshot)
	if found {
		p.log.Info("tenant deprovisioned", "world", slug, "bucketDeleted", deleteBucket)
	}
	return found, nil
}

// applySnapshot publishes a registry snapshot to this pod's world set.
func (p *Provisioner) applySnapshot(registry *tenantRegistry) {
	p.cfg.SetDynamicWorlds(p.worldsFromRegistry(registry))
}

func (c *Config) registryRef() SecretRef {
	return SecretRef{
		Namespace: c.Server.BrokerNamespace,
		Name:      c.Provisioning.RegistrySecret,
		Key:       registrySecretKey,
	}
}

func (c *Config) worldsFragmentRef() SecretRef {
	return SecretRef{
		Namespace: c.Provisioning.ServerNamespace,
		Name:      c.Provisioning.WorldsSecret,
		Key:       worldsFragmentKey,
	}
}

// identityKey is the stable provisioning identity: the CONFIGURED IdP
// issuer plus token subject (a token's own iss varies between IdP-signed
// and broker-signed paths; email is display identity and may change).
func identityKey(issuer, subject string) string {
	return issuer + "|" + subject
}

// tenantSlug derives the world name: sanitized email local part plus a
// short identity hash. Readable in every mark:// URL, collision-free
// via the suffix, stable because the suffix depends on issuer|sub only.
func tenantSlug(issuer, subject, email string) string {
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
	sum := sha256.Sum256([]byte(identityKey(issuer, subject)))
	return cleaned + "-" + hex.EncodeToString(sum[:4])
}

// tenantWorldID derives a deterministic canonical UUID (version 4
// shape, RFC 4122 variant) from the identity, so concurrent
// provisioners mint the same worldID without coordination.
func tenantWorldID(issuer, subject string) string {
	sum := sha256.Sum256([]byte("worldid|" + identityKey(issuer, subject)))
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
func (p *Provisioner) admits(claims *Claims) error {
	switch p.cfg.Provisioning.Mode {
	case ProvisionOpen:
		return nil
	case ProvisionAllowlisted:
		if worldAllows(&p.cfg.Provisioning.Allow, claims) {
			return nil
		}
		return ErrProvisioningDenied
	default:
		return ErrProvisioningDenied
	}
}

// EnsureTenant provisions (or converges) the caller's world and returns
// its broker-side WorldConfig. Every step is idempotent and keyed by
// the identity-derived slug; any replica can re-run it safely.
func (p *Provisioner) EnsureTenant(ctx context.Context, claims *Claims) (world WorldConfig, err error) {
	created := false
	p.mu.Lock()
	defer p.mu.Unlock()
	// The lock queue can outlive the request; do no remote work for a
	// caller that already went away.
	if err := ctx.Err(); err != nil {
		return WorldConfig{}, err
	}

	email := canonicalEmail(claims.Email)
	issuer := p.cfg.OIDC.Issuer
	slug := tenantSlug(issuer, claims.Subject, email)
	worldID := tenantWorldID(issuer, claims.Subject)

	// Step 1: registry entry (the create decision lives here, under the
	// Secret's optimistic concurrency).
	var snapshot tenantRegistry
	err = p.store.Mutate(ctx, p.cfg.registryRef(), func(existing []byte) ([]byte, error) {
		registry, decodeErr := decodeRegistry(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if _, ok := registry.Tenants[slug]; !ok {
			if gateErr := p.admits(claims); gateErr != nil {
				return nil, gateErr
			}
			maxTenants := p.cfg.Provisioning.MaxTenants
			if maxTenants > 0 && len(registry.Tenants) >= maxTenants {
				return nil, ErrTenantCapacity
			}
			registry.Tenants[slug] = tenantRecord{
				Email:   email,
				Issuer:  issuer,
				Subject: claims.Subject,
				WorldID: worldID,
				Created: p.clock().UTC().Format(time.RFC3339),
			}
			created = true
		}
		snapshot = registry
		encoded, encErr := json.Marshal(&registry)
		if encErr != nil {
			return nil, fmt.Errorf("encode tenant registry: %w", encErr)
		}
		return encoded, nil
	})
	if err != nil {
		return WorldConfig{}, err
	}

	// No already-provisioned fast path here: the registry sync can
	// publish a world whose bucket/fragment ensure crashed mid-way, so
	// every EnsureTenant re-runs the idempotent steps to converge.

	// Step 2: backend storage. EnsureBucket is idempotent.
	if err := p.buckets.EnsureBucket(ctx, p.bucketName(slug)); err != nil {
		return WorldConfig{}, fmt.Errorf("ensure bucket for %q: %w", slug, err)
	}

	// Step 3: tokens key, BEFORE the fragment: the server opens the
	// world only once its tokens file exists in the shared mount.
	world = p.cfg.Provisioning.tenantWorld(slug, email)
	if err := p.ensureTokensKey(ctx, &world, slug); err != nil {
		return WorldConfig{}, err
	}

	// Step 4: worlds fragment, rendered as a UNION with the existing
	// fragment so a stale snapshot never drops a sibling's tenant;
	// removal is a deliberate future flow rewriting from the registry.
	if err := p.store.Mutate(ctx, p.cfg.worldsFragmentRef(), func(existing []byte) ([]byte, error) {
		return p.renderWorldsFragmentUnion(&snapshot, existing)
	}); err != nil {
		return WorldConfig{}, fmt.Errorf("write worlds fragment: %w", err)
	}

	// Step 5: make the tenant visible to this pod immediately; other
	// replicas converge via the registry sync loop.
	p.applySnapshot(&snapshot)
	if created {
		p.log.Info("tenant provisioned", "world", slug, "subject", hashSubject(claims.Subject))
	}
	return world, nil
}

func (p *Provisioner) ensureTokensKey(ctx context.Context, world *WorldConfig, slug string) error {
	return p.store.Mutate(ctx, p.cfg.worldTokensRef(world), func(existing []byte) ([]byte, error) {
		if len(existing) > 0 {
			return existing, nil
		}
		// Non-empty placeholder: the store contract skips empty writes,
		// and the server needs the file present to open the world.
		return []byte("# demarkus world tokens for " + slug + "\n"), nil
	})
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

// fragmentWorld is the knowledge-server worlds-fragment YAML shape
// (mirrors knowledgeconfig's rawWorldConfig subset the server parses;
// the kind harness's producer-consumer test pins the contract).
type fragmentLimits struct {
	MaxDocuments int `yaml:"maxDocuments"`
}

// worldsFragmentDoc is the fragment's on-disk document shape.
type worldsFragmentDoc struct {
	Worlds []fragmentWorld `yaml:"worlds"`
}

type fragmentWorld struct {
	Name        string   `yaml:"name"`
	Authorities []string `yaml:"authorities"`
	Bucket      struct {
		URL     string `yaml:"url"`
		WorldID string `yaml:"worldID"`
	} `yaml:"bucket"`
	Auth struct {
		TokensFile string `yaml:"tokensFile"`
	} `yaml:"auth"`
	Limits    *fragmentLimits `yaml:"limits,omitempty"`
	Bootstrap bool            `yaml:"bootstrap"`
}

// renderWorldsFragmentAuthoritative renders exactly the registry's
// worlds: the ONE mode allowed to drop a world (deprovision).
func (p *Provisioner) renderWorldsFragmentAuthoritative(registry *tenantRegistry) ([]byte, error) {
	return p.renderWorldsFragmentUnion(registry, nil)
}

// renderWorldsFragmentUnion renders the union of the existing fragment
// and the registry snapshot (snapshot wins per slug), sorted by slug:
// a stale snapshot can never drop a sibling's tenant.
func (p *Provisioner) renderWorldsFragmentUnion(registry *tenantRegistry, existing []byte) ([]byte, error) {
	worlds := map[string]fragmentWorld{}
	if len(existing) > 0 {
		var current worldsFragmentDoc
		if err := yaml.Unmarshal(existing, &current); err != nil {
			return nil, fmt.Errorf("parse existing worlds fragment: %w", err)
		}
		for _, world := range current.Worlds {
			worlds[world.Name] = world
		}
	}
	for slug, record := range registry.Tenants {
		world := fragmentWorld{
			Name:        slug,
			Authorities: []string{slug + "." + p.cfg.Provisioning.AuthorityDomain},
			Bootstrap:   true,
		}
		world.Bucket.URL = "gs://" + p.bucketName(slug)
		world.Bucket.WorldID = record.WorldID
		world.Auth.TokensFile = strings.TrimRight(p.cfg.Provisioning.TokensMountPath, "/") + "/" + slug + ".toml"
		if quota := p.cfg.Provisioning.MaxDocumentsPerTenant; quota > 0 {
			world.Limits = &fragmentLimits{MaxDocuments: quota}
		}
		worlds[slug] = world
	}
	doc := worldsFragmentDoc{Worlds: make([]fragmentWorld, 0, len(worlds))}
	for _, name := range slices.Sorted(maps.Keys(worlds)) {
		doc.Worlds = append(doc.Worlds, worlds[name])
	}
	return yaml.Marshal(doc)
}

// worldsFromRegistry renders the broker-side dynamic world set.
func (p *Provisioner) worldsFromRegistry(registry *tenantRegistry) []WorldConfig {
	slugs := sortedSlugs(registry)
	worlds := make([]WorldConfig, 0, len(slugs))
	for _, slug := range slugs {
		worlds = append(worlds, p.cfg.Provisioning.tenantWorld(slug, registry.Tenants[slug].Email))
	}
	return worlds
}

// sortedSlugs is the one ordering rule for registry projections (the
// fragment golden fixture depends on it).
func sortedSlugs(registry *tenantRegistry) []string {
	slugs := make([]string, 0, len(registry.Tenants))
	for slug := range registry.Tenants {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// SyncRegistry reads the registry and republishes the dynamic world
// set (skipping unchanged payloads), entirely under p.mu so a stale
// snapshot can never overwrite a concurrent local mutation's state.
func (p *Provisioner) SyncRegistry(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var snapshot tenantRegistry
	var payload [32]byte
	err := p.store.Mutate(ctx, p.cfg.registryRef(), func(existing []byte) ([]byte, error) {
		registry, decodeErr := decodeRegistry(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		snapshot = registry
		payload = sha256.Sum256(existing)
		return existing, nil // read-only pass; unchanged data is not rewritten
	})
	if err != nil {
		return err
	}
	if payload == p.lastSync {
		return nil
	}
	p.lastSync = payload
	p.applySnapshot(&snapshot)
	return nil
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
