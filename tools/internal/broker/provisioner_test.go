package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// provisionerStore backs provisioner tests with the real Kubernetes
// SecretStore over a fake clientset, so the optimistic-concurrency and
// create-when-absent semantics under test are the production ones.
type provisionerStore struct {
	SecretStore
	clientset *fake.Clientset
}

func newProvisionerStore() *provisionerStore {
	clientset := fake.NewSimpleClientset()
	return &provisionerStore{SecretStore: NewK8sSecretStore(clientset), clientset: clientset}
}

func (s *provisionerStore) get(ref SecretRef) []byte {
	secret, err := s.clientset.CoreV1().Secrets(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return secret.Data[ref.Key]
}

// fakeBuckets records EnsureBucket calls; fail makes every call error.
type fakeBuckets struct {
	mu      sync.Mutex
	created []string
	deleted []string
	fail    bool
}

func (f *fakeBuckets) EnsureBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("bucket backend down")
	}
	f.created = append(f.created, bucket)
	return nil
}

func provisioningTestConfig(mode string) *Config {
	cfg := memoryTestConfig()
	cfg.Provisioning = ProvisioningConfig{
		Mode:            mode,
		MaxTenants:      10,
		AuthorityDomain: "memory-worlds.svc.cluster.local",
		DialAddress:     "demarkus-knowledge-server.memory-worlds.svc.cluster.local:6309",
		BucketPrefix:    "gs://memory-",
		BucketProject:   "demarkus-test",
		ServerNamespace: "memory-worlds",
		WorldsSecret:    "memory-worlds-config",
		TokensSecret:    "memory-worlds-tokens",
		TokensMountPath: "/etc/demarkus/worlds-tokens",
		RegistrySecret:  "memory-broker-registry",
	}
	return cfg
}

func newTestProvisioner(cfg *Config, store SecretStore, buckets BucketCreator) *Provisioner {
	return &Provisioner{
		cfg:     cfg,
		store:   store,
		buckets: buckets,
		log:     slog.Default(),
		clock:   func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) },
	}
}

func eveClaims() *Claims {
	return &Claims{Subject: "google|eve-123", Email: "Eve.Adams@example.com", EmailVerified: true}
}

func TestTenantSlugShape(t *testing.T) {
	slug := tenantSlug("https://idp", "google|eve-123", "eve.adams@example.com")
	if !strings.HasPrefix(slug, "eve-adams-") {
		t.Errorf("slug = %q, want eve-adams- prefix", slug)
	}
	if len(strings.Split(slug, "-")) < 3 || len(slug) > 63 {
		t.Errorf("slug %q not a safe DNS label", slug)
	}
	// Stable across email display changes only via issuer|sub suffix:
	// same identity, same suffix.
	again := tenantSlug("https://idp", "google|eve-123", "eve.adams@example.com")
	if slug != again {
		t.Errorf("slug not deterministic: %q vs %q", slug, again)
	}
	// Different subject, different slug.
	other := tenantSlug("https://idp", "google|mallory", "eve.adams@example.com")
	if other == slug {
		t.Error("distinct subjects produced identical slugs")
	}
	// Hostile local parts sanitize to a DNS-safe label.
	weird := tenantSlug("https://idp", "s", "--Ünsafe..name!!@example.com")
	label := strings.Split(weird, "-")[0]
	if label == "" || strings.HasPrefix(weird, "-") || strings.Contains(weird, "--") {
		t.Errorf("hostile local part produced unsafe slug %q", weird)
	}
}

func TestTenantWorldIDIsCanonicalUUID(t *testing.T) {
	id := tenantWorldID("https://idp", "google|eve-123")
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("worldID %q is not UUID-shaped", id)
	}
	if id[14] != '4' {
		t.Errorf("worldID %q version nibble = %c, want 4", id, id[14])
	}
	if !strings.ContainsRune("89ab", rune(id[19])) {
		t.Errorf("worldID %q variant nibble = %c, want RFC 4122", id, id[19])
	}
	if id != tenantWorldID("https://idp", "google|eve-123") {
		t.Error("worldID not deterministic")
	}
}

func TestEnsureTenantProvisionsAndConverges(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	buckets := &fakeBuckets{}
	p := newTestProvisioner(cfg, store, buckets)

	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !strings.HasPrefix(world.Name, "eve-adams-") {
		t.Errorf("world name = %q", world.Name)
	}
	if world.DialAddress != cfg.Provisioning.DialAddress || world.TokensSecretKey != world.Name+".toml" {
		t.Errorf("world template wrong: %+v", world)
	}
	if len(world.Allow.Emails) != 1 || world.Allow.Emails[0] != "eve.adams@example.com" {
		t.Errorf("world allow = %+v, want the canonical email", world.Allow)
	}

	// Bucket created with prefix.
	if len(buckets.created) != 1 || buckets.created[0] != "memory-"+world.Name {
		t.Errorf("buckets created = %v", buckets.created)
	}
	// Tokens key exists and is non-empty.
	if len(store.get(cfg.worldTokensRef(&world))) == 0 {
		t.Error("tokens key was not seeded")
	}
	// Fragment rendered with the world, bootstrap on.
	fragment := string(store.get(cfg.worldsFragmentRef()))
	for _, want := range []string{"name: " + world.Name, "bootstrap: true", "gs://memory-" + world.Name, world.Name + ".toml", world.Name + ".memory-worlds.svc.cluster.local"} {
		if !strings.Contains(fragment, want) {
			t.Errorf("fragment missing %q:\n%s", want, fragment)
		}
	}
	// Dynamic world visible to this pod's config immediately.
	if _, ok := cfg.FindWorld(world.Name); !ok {
		t.Error("provisioned world not in dynamic set")
	}

	// Second call: pure convergence, no new bucket entries beyond the
	// idempotent re-ensure, same world.
	again, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant (second): %v", err)
	}
	if again.Name != world.Name {
		t.Errorf("second EnsureTenant world = %q, want %q", again.Name, world.Name)
	}
	registry := string(store.get(cfg.registryRef()))
	if strings.Count(registry, `"email"`) != 1 {
		t.Errorf("registry has duplicate tenants:\n%s", registry)
	}
}

// movedEveClaims is eve after an email change touching both the local
// part and the domain: the recomputed slug would differ entirely.
func movedEveClaims() *Claims {
	return &Claims{Subject: "google|eve-123", Email: "Eve.Moved@elsewhere.io", EmailVerified: true}
}

// registryUpdateCount counts Secret update writes against the tenant
// registry, so tests can assert exactly when a refresh persists.
func registryUpdateCount(store *provisionerStore, cfg *Config) int {
	count := 0
	for _, action := range store.clientset.Actions() {
		update, ok := action.(k8stesting.UpdateAction)
		if !ok || action.GetVerb() != "update" {
			continue
		}
		secret, ok := update.GetObject().(*corev1.Secret)
		if !ok || secret.Name != cfg.Provisioning.RegistrySecret {
			continue
		}
		count++
	}
	return count
}

// TestEnsureTenantEmailChangeKeepsWorld: an email change (new local
// part AND domain) resolves to the ORIGINAL world; no second world
// appears anywhere and the record's email is refreshed.
func TestEnsureTenantEmailChangeKeepsWorld(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	original, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}

	world, err := p.EnsureTenant(context.Background(), movedEveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant after email change: %v", err)
	}
	if world.Name != original.Name {
		t.Errorf("email change moved the world: %q -> %q", original.Name, world.Name)
	}
	if len(world.Allow.Emails) != 1 || world.Allow.Emails[0] != "eve.moved@elsewhere.io" {
		t.Errorf("returned world allow = %+v, want the new canonical email", world.Allow)
	}

	registry := string(store.get(cfg.registryRef()))
	if strings.Count(registry, `"email"`) != 1 {
		t.Errorf("email change duplicated the tenant:\n%s", registry)
	}
	if !strings.Contains(registry, "eve.moved@elsewhere.io") || strings.Contains(registry, "eve.adams@example.com") {
		t.Errorf("record email not refreshed:\n%s", registry)
	}
	fragment := string(store.get(cfg.worldsFragmentRef()))
	if strings.Count(fragment, "name: ") != 1 {
		t.Errorf("email change grew the fragment:\n%s", fragment)
	}
	// Dynamic world set converged on the new email for this pod.
	w, ok := cfg.FindWorld(original.Name)
	if !ok {
		t.Fatal("original world missing from dynamic set")
	}
	if len(w.Allow.Emails) != 1 || w.Allow.Emails[0] != "eve.moved@elsewhere.io" {
		t.Errorf("dynamic world allow = %+v, want the new email", w.Allow)
	}
}

// TestEnsureTenantEmailRefreshOnceThenFastPath: the refresh writes the
// registry exactly once, after which tenantWorldFor resolves the tenant
// without EnsureTenant.
func TestEnsureTenantEmailRefreshOnceThenFastPath(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatal(err)
	}

	// Stale record: identity index hits but Allow rejects the new email,
	// denying so the slow path runs once and refreshes.
	if _, err := tenantWorldFor(cfg, movedEveClaims()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("pre-refresh tenantWorldFor err = %v, want ErrNotAuthorized", err)
	}

	before := registryUpdateCount(store, cfg)
	world, err := p.EnsureTenant(context.Background(), movedEveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if got := registryUpdateCount(store, cfg); got != before+1 {
		t.Errorf("registry updates during refresh = %d, want exactly 1", got-before)
	}

	// Fast path: the resolver answers by identity index, no provisioner.
	w, err := tenantWorldFor(cfg, movedEveClaims())
	if err != nil {
		t.Fatalf("post-refresh tenantWorldFor: %v", err)
	}
	if w.Name != world.Name {
		t.Errorf("fast path world = %q, want %q", w.Name, world.Name)
	}

	// A repeat EnsureTenant is a no-op on the registry.
	after := registryUpdateCount(store, cfg)
	if _, err := p.EnsureTenant(context.Background(), movedEveClaims()); err != nil {
		t.Fatal(err)
	}
	if got := registryUpdateCount(store, cfg); got != after {
		t.Errorf("converged EnsureTenant wrote the registry %d more times", got-after)
	}
}

// TestEnsureTenantSharedLocalPartDistinctWorlds: two identities whose
// emails share a local part keep distinct worlds and resolve to their
// own.
func TestEnsureTenantSharedLocalPartDistinctWorlds(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	eveWorld, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	twin := &Claims{Subject: "google|mallory", Email: "eve.adams@other.org", EmailVerified: true}
	twinWorld, err := p.EnsureTenant(context.Background(), twin)
	if err != nil {
		t.Fatal(err)
	}
	if twinWorld.Name == eveWorld.Name {
		t.Fatalf("shared local part collapsed two identities into %q", eveWorld.Name)
	}
	w, err := tenantWorldFor(cfg, twin)
	if err != nil {
		t.Fatalf("tenantWorldFor(twin): %v", err)
	}
	if w.Name != twinWorld.Name {
		t.Errorf("twin resolved to %q, want %q", w.Name, twinWorld.Name)
	}
}

// TestEnsureTenantRaceEmailChangeConverges: two provisioners racing on
// one identity, one carrying a changed email, converge on one slug.
func TestEnsureTenantRaceEmailChangeConverges(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	raw := newProvisionerStore()
	hooked := &hookedStore{SecretStore: raw, ref: cfg.registryRef()}
	p := newTestProvisioner(cfg, hooked, &fakeBuckets{})
	sibling := newTestProvisioner(provisioningTestConfig(ProvisionOpen), raw, &fakeBuckets{})

	// Right before p's registry mutate: the sibling provisions eve under
	// her original email, so p's create decision must become a hit.
	var originalWorld WorldConfig
	hooked.hook = func() {
		var hookErr error
		if originalWorld, hookErr = sibling.EnsureTenant(context.Background(), eveClaims()); hookErr != nil {
			t.Errorf("racing EnsureTenant(original email): %v", hookErr)
		}
	}
	world, err := p.EnsureTenant(context.Background(), movedEveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if world.Name != originalWorld.Name {
		t.Errorf("race produced two slugs: %q vs %q", world.Name, originalWorld.Name)
	}
	registry := string(raw.get(cfg.registryRef()))
	if strings.Count(registry, `"email"`) != 1 {
		t.Errorf("race duplicated the tenant:\n%s", registry)
	}
	if !strings.Contains(registry, "eve.moved@elsewhere.io") {
		t.Errorf("race lost the email refresh:\n%s", registry)
	}
}

// TestEnsureTenantTombstonedDeniedAfterEmailChange: the tombstone still
// blocks re-provisioning when the caller's email has changed.
func TestEnsureTenantTombstonedDeniedAfterEmailChange(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	raw := newProvisionerStore()
	failing := &hookedStore{SecretStore: raw, ref: cfg.worldsFragmentRef()}
	p := newTestProvisioner(cfg, failing, &fakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	// Crash the deprovision after phase 1 so the tombstone persists.
	failing.hook = func() { panic(errAbortMutate) }
	var recovered any
	var deprovisionErr error
	func() {
		defer func() { recovered = recover() }()
		_, deprovisionErr = p.DeprovisionTenant(context.Background(), world.Name, false)
	}()
	if recoveredErr, ok := recovered.(error); !ok || !errors.Is(recoveredErr, errAbortMutate) {
		t.Fatalf("recovered %v (deprovision err %v), want the injected abort panic", recovered, deprovisionErr)
	}

	if _, err := p.EnsureTenant(context.Background(), movedEveClaims()); !errors.Is(err, ErrTenantDeprovisioning) {
		t.Fatalf("err = %v, want ErrTenantDeprovisioning", err)
	}
}

// TestLegacyDuplicateIdentityResolvesDeterministically: two records for
// one identity (the pre-pinning orphan bug) resolve identically on both
// paths; tombstoning the pinned duplicate moves both to the live one.
func TestLegacyDuplicateIdentityResolvesDeterministically(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})

	seedRegistry := func(registry *tenantRegistry) {
		encoded, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Mutate(context.Background(), cfg.registryRef(), func([]byte) ([]byte, error) {
			return encoded, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	record := func(email string, deleting bool) tenantRecord {
		return tenantRecord{
			Email: email, Issuer: cfg.OIDC.Issuer, Subject: "google|eve-123",
			WorldID: tenantWorldID(cfg.OIDC.Issuer, "google|eve-123"), Deleting: deleting,
		}
	}
	seedRegistry(&tenantRegistry{Tenants: map[string]tenantRecord{
		"a-eve-0000": record("eve.adams@example.com", false),
		"b-eve-1111": record("eve.old@example.com", false),
	}})

	// Both paths pin the first sorted slug.
	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if world.Name != "a-eve-0000" {
		t.Errorf("EnsureTenant resolved %q, want the first sorted slug a-eve-0000", world.Name)
	}
	w, err := tenantWorldFor(cfg, eveClaims())
	if err != nil {
		t.Fatalf("tenantWorldFor: %v", err)
	}
	if w.Name != world.Name {
		t.Errorf("fast path resolved %q, EnsureTenant %q", w.Name, world.Name)
	}

	// Tombstoning the pinned duplicate (the orphan-cleanup flow) moves
	// resolution to the live record instead of denying the tenant.
	seedRegistry(&tenantRegistry{Tenants: map[string]tenantRecord{
		"a-eve-0000": record("eve.adams@example.com", true),
		"b-eve-1111": record("eve.old@example.com", false),
	}})
	world, err = p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if world.Name != "b-eve-1111" {
		t.Errorf("EnsureTenant with tombstoned duplicate resolved %q, want b-eve-1111", world.Name)
	}
	w, err = tenantWorldFor(cfg, eveClaims())
	if err != nil {
		t.Fatalf("tenantWorldFor with tombstoned duplicate: %v", err)
	}
	if w.Name != world.Name {
		t.Errorf("fast path resolved %q, EnsureTenant %q", w.Name, world.Name)
	}
}

func TestEnsureTenantGateModes(t *testing.T) {
	t.Run("static mode denies", func(t *testing.T) {
		cfg := provisioningTestConfig(ProvisionStatic)
		p := newTestProvisioner(cfg, newProvisionerStore(), &fakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), eveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
	})
	t.Run("allowlisted admits matching identity", func(t *testing.T) {
		cfg := provisioningTestConfig(ProvisionAllowlisted)
		cfg.Provisioning.Allow = AllowConfig{Domains: []string{"example.com"}}
		p := newTestProvisioner(cfg, newProvisionerStore(), &fakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
			t.Fatalf("EnsureTenant: %v", err)
		}
	})
	t.Run("allowlisted denies others", func(t *testing.T) {
		cfg := provisioningTestConfig(ProvisionAllowlisted)
		cfg.Provisioning.Allow = AllowConfig{Domains: []string{"other.org"}}
		p := newTestProvisioner(cfg, newProvisionerStore(), &fakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), eveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
	})
	t.Run("existing tenant bypasses a tightened gate", func(t *testing.T) {
		cfg := provisioningTestConfig(ProvisionOpen)
		store := newProvisionerStore()
		p := newTestProvisioner(cfg, store, &fakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		cfg.Provisioning.Mode = ProvisionAllowlisted
		cfg.Provisioning.Allow = AllowConfig{Domains: []string{"other.org"}}
		if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
			t.Fatalf("existing tenant locked out by tightened gate: %v", err)
		}
	})
}

func TestEnsureTenantCapacity(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	cfg.Provisioning.MaxTenants = 1
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatalf("first tenant: %v", err)
	}
	second := &Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	if _, err := p.EnsureTenant(context.Background(), second); !errors.Is(err, ErrTenantCapacity) {
		t.Fatalf("err = %v, want ErrTenantCapacity", err)
	}
	// The existing tenant still converges at capacity.
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatalf("existing tenant blocked at capacity: %v", err)
	}
}

func TestEnsureTenantBucketFailureRetries(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	buckets := &fakeBuckets{fail: true}
	p := newTestProvisioner(cfg, store, buckets)
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err == nil {
		t.Fatal("EnsureTenant must surface bucket failure")
	}
	// Registry already holds the tenant; a retry converges the rest.
	buckets.fail = false
	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatalf("retry after bucket failure: %v", err)
	}
	if len(store.get(cfg.worldsFragmentRef())) == 0 {
		t.Error("fragment missing after converged retry")
	}
	if _, ok := cfg.FindWorld(world.Name); !ok {
		t.Error("world not visible after converged retry")
	}
}

func TestSyncRegistryPublishesDynamicWorlds(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatal(err)
	}

	// A sibling replica sees the tenant purely via registry sync.
	sibling := provisioningTestConfig(ProvisionOpen)
	q := newTestProvisioner(sibling, store, &fakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	slug := tenantSlug(cfg.OIDC.Issuer, "google|eve-123", "eve.adams@example.com")
	if _, ok := sibling.FindWorld(slug); !ok {
		t.Errorf("sibling did not converge on tenant %q", slug)
	}
}

func TestFragmentRendersDocumentQuota(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	cfg.Provisioning.MaxDocumentsPerTenant = 500
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatal(err)
	}
	fragment := string(store.get(cfg.worldsFragmentRef()))
	if !strings.Contains(fragment, "maxDocuments: 500") {
		t.Errorf("fragment missing per-tenant quota:\n%s", fragment)
	}
}

// TestWorldsFragmentGolden pins the rendered fragment as the
// producer-consumer contract fixture the server's knowledgeconfig test
// parses; a shape drift breaks one side visibly.
func TestWorldsFragmentGolden(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	cfg.Provisioning.MaxDocumentsPerTenant = 500
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Fatal(err)
	}
	rendered := store.get(cfg.worldsFragmentRef())

	golden := filepath.Join("testdata", "worlds-fragment.golden.yaml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, rendered, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if !bytes.Equal(want, rendered) {
		t.Errorf("rendered fragment drifted from golden; if intended, rerun with UPDATE_GOLDEN=1 and check the server-side parse test\nrendered:\n%s", rendered)
	}
}

func TestValidateProvisioningTrimsMode(t *testing.T) {
	cfg := provisioningTestConfig("open ")
	if err := cfg.ValidateProvisioning(); err != nil {
		t.Fatalf("ValidateProvisioning: %v", err)
	}
	if cfg.Provisioning.Mode != ProvisionOpen {
		t.Errorf("mode = %q, want normalized %q (Enabled and the open-mode cap key on it)", cfg.Provisioning.Mode, ProvisionOpen)
	}

	cfg = provisioningTestConfig("open ")
	cfg.Provisioning.MaxTenants = 0
	if err := cfg.ValidateProvisioning(); err == nil {
		t.Error("padded open mode bypassed the maxTenants requirement")
	}
}

// TestFragmentUnionSurvivesStaleSnapshot: a replica holding a stale
// registry snapshot must not drop a tenant a sibling published into the
// fragment between the registry read and the fragment write.
func TestFragmentUnionSurvivesStaleSnapshot(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	sibling := newTestProvisioner(provisioningTestConfig(ProvisionOpen), store, &fakeBuckets{})

	// Sibling provisions frank first; the fragment now carries his world.
	frank := &Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	frankWorld, err := sibling.EnsureTenant(context.Background(), frank)
	if err != nil {
		t.Fatal(err)
	}

	// Provision eve, then simulate the worst case: render from an
	// eve-only STALE snapshot over the live fragment; frank must survive.
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	eveWorld, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	stale := &tenantRegistry{Tenants: map[string]tenantRecord{
		eveWorld.Name: {Email: "eve.adams@example.com", WorldID: tenantWorldID(cfg.OIDC.Issuer, "google|eve-123")},
	}}
	merged, err := p.renderWorldsFragmentUnion(stale, store.get(cfg.worldsFragmentRef()))
	if err != nil {
		t.Fatalf("union render: %v", err)
	}
	var doc worldsFragmentDoc
	if err := yaml.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("parse union fragment: %v", err)
	}
	got := make([]string, 0, len(doc.Worlds))
	for _, world := range doc.Worlds {
		got = append(got, world.Name)
	}
	// Exact sorted set: no drops, no duplicates, and the deterministic
	// order Mutate's unchanged-bytes suppression depends on.
	want := []string{eveWorld.Name, frankWorld.Name}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("union fragment worlds = %v, want %v", got, want)
	}
}

func (f *fakeBuckets) DeleteBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, bucket)
	return nil
}

// hookedStore fires hook once, right before the first Mutate of ref,
// to interleave a concurrent actor at an exact point in a flow.
type hookedStore struct {
	SecretStore
	mu   sync.Mutex
	ref  SecretRef
	hook func()
}

func (h *hookedStore) Mutate(ctx context.Context, ref SecretRef, fn func([]byte) ([]byte, error)) error {
	h.mu.Lock()
	hook := h.hook
	if hook != nil && ref == h.ref {
		h.hook = nil
	} else {
		hook = nil
	}
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return h.SecretStore.Mutate(ctx, ref, fn)
}

// TestDeprovisionPreservesRacingFragmentEntry: a world provisioned
// AFTER the deprovision's registry snapshot survives the fragment
// rewrite; the tombstoned slug stays blocked until cleanup finishes.
func TestDeprovisionPreservesRacingFragmentEntry(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	raw := newProvisionerStore()
	hooked := &hookedStore{SecretStore: raw, ref: cfg.worldsFragmentRef()}
	p := newTestProvisioner(cfg, hooked, &fakeBuckets{})
	eveWorld, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}

	// Between p's registry tombstone and its fragment rewrite: a sibling
	// provisions frank (postdating p's snapshot) and eve tries to come
	// back (must be blocked by the tombstone).
	sibling := newTestProvisioner(provisioningTestConfig(ProvisionOpen), raw, &fakeBuckets{})
	frank := &Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	var frankWorld WorldConfig
	hooked.hook = func() {
		var hookErr error
		if frankWorld, hookErr = sibling.EnsureTenant(context.Background(), frank); hookErr != nil {
			t.Errorf("racing EnsureTenant(frank): %v", hookErr)
		}
		if _, hookErr = sibling.EnsureTenant(context.Background(), eveClaims()); !errors.Is(hookErr, ErrTenantDeprovisioning) {
			t.Errorf("EnsureTenant on tombstoned slug err = %v, want ErrTenantDeprovisioning", hookErr)
		}
	}

	if _, err := p.DeprovisionTenant(context.Background(), eveWorld.Name, false); err != nil {
		t.Fatalf("DeprovisionTenant: %v", err)
	}
	fragment := string(raw.get(cfg.worldsFragmentRef()))
	if strings.Contains(fragment, "name: "+eveWorld.Name) {
		t.Errorf("deprovisioned world survived the fragment rewrite:\n%s", fragment)
	}
	if !strings.Contains(fragment, "name: "+frankWorld.Name) {
		t.Errorf("racing sibling world dropped by deprovision:\n%s", fragment)
	}
	// Tombstone cleared: eve can provision again.
	if _, err := sibling.EnsureTenant(context.Background(), eveClaims()); err != nil {
		t.Errorf("re-provision after completed deprovision: %v", err)
	}
}

// TestSyncRemovesTombstonedWorld: a crashed deprovision leaves the
// tombstone; the sync loop must still take the world out of service.
func TestSyncRemovesTombstonedWorld(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	raw := newProvisionerStore()
	// Crash simulation: fail the tombstone's fragment rewrite so
	// DeprovisionTenant exits after phase 1.
	failing := &hookedStore{SecretStore: raw, ref: cfg.worldsFragmentRef()}
	p := newTestProvisioner(cfg, failing, &fakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	failing.hook = func() { panic(errAbortMutate) }
	func() {
		defer func() { _ = recover() }()
		_, _ = p.DeprovisionTenant(context.Background(), world.Name, false)
	}()

	// Registry holds the tombstone, fragment still holds the world; a
	// sibling's sync must drop it from both projections.
	sibling := provisioningTestConfig(ProvisionOpen)
	q := newTestProvisioner(sibling, raw, &fakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	fragment := string(raw.get(cfg.worldsFragmentRef()))
	if strings.Contains(fragment, "name: "+world.Name) {
		t.Errorf("sync kept a tombstoned world in the fragment:\n%s", fragment)
	}
	if _, ok := sibling.FindWorld(world.Name); ok {
		t.Error("sync kept a tombstoned world resolvable")
	}
}

var errAbortMutate = errors.New("test: abort mutate")

// TestSyncRegistryConvergesFragment: the sync loop repairs a fragment
// that lost a registry-held world to a stale rewrite.
func TestSyncRegistryConvergesFragment(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &fakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the stale drop: empty the fragment while the registry
	// still holds the tenant.
	if err := p.store.Mutate(context.Background(), cfg.worldsFragmentRef(), func([]byte) ([]byte, error) {
		return []byte("worlds: []\n"), nil
	}); err != nil {
		t.Fatal(err)
	}

	sibling := provisioningTestConfig(ProvisionOpen)
	q := newTestProvisioner(sibling, store, &fakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	fragment := string(store.get(cfg.worldsFragmentRef()))
	if !strings.Contains(fragment, "name: "+world.Name) {
		t.Errorf("sync did not restore the dropped world:\n%s", fragment)
	}
}

func TestDeprovisionTenantRemovesEverything(t *testing.T) {
	cfg := provisioningTestConfig(ProvisionOpen)
	store := newProvisionerStore()
	buckets := &fakeBuckets{}
	p := newTestProvisioner(cfg, store, buckets)

	world, err := p.EnsureTenant(context.Background(), eveClaims())
	if err != nil {
		t.Fatal(err)
	}
	frank := &Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	frankWorld, err := p.EnsureTenant(context.Background(), frank)
	if err != nil {
		t.Fatal(err)
	}

	found, err := p.DeprovisionTenant(context.Background(), world.Name, true)
	if err != nil {
		t.Fatalf("DeprovisionTenant: %v", err)
	}
	if !found {
		t.Fatal("DeprovisionTenant reported the live tenant as absent")
	}

	// Registry no longer holds eve; frank survives.
	registry := string(store.get(cfg.registryRef()))
	if strings.Contains(registry, world.Name) || !strings.Contains(registry, frankWorld.Name) {
		t.Errorf("registry after deprovision:\n%s", registry)
	}
	// Fragment rewrite dropped eve's world (the one flow allowed to).
	fragment := string(store.get(cfg.worldsFragmentRef()))
	if strings.Contains(fragment, "name: "+world.Name) || !strings.Contains(fragment, "name: "+frankWorld.Name) {
		t.Errorf("fragment after deprovision:\n%s", fragment)
	}
	// Tokens key and write-token record deleted.
	if got := store.get(cfg.worldTokensRef(&world)); len(got) != 0 {
		t.Errorf("tokens key survived deprovision: %q", got)
	}
	if got := store.get(cfg.worldWriteTokenRef(world.Name)); len(got) != 0 {
		t.Errorf("write-token record survived deprovision: %q", got)
	}
	// Bucket deleted on request.
	buckets.mu.Lock()
	deleted := append([]string(nil), buckets.deleted...)
	buckets.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "memory-"+world.Name {
		t.Errorf("deleted buckets = %v", deleted)
	}
	// Dynamic world set no longer resolves eve.
	if _, ok := cfg.FindWorld(world.Name); ok {
		t.Error("deprovisioned world still resolvable")
	}

	// Rerun converges: no error, reports the tenant as already absent.
	found, err = p.DeprovisionTenant(context.Background(), world.Name, false)
	if err != nil {
		t.Errorf("rerun err = %v, want nil (convergence)", err)
	}
	if found {
		t.Error("rerun reported the deprovisioned tenant as found")
	}
}
