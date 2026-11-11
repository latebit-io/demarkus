package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/brokertest"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/knowledgeserver/worldsfragment"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// provisionerStore backs provisioner tests with the real Kubernetes
// SecretStore over a fake clientset, so the optimistic-concurrency and
// create-when-absent semantics under test are the production ones.
type provisionerStore struct {
	core.SecretStore
	clientset *fake.Clientset
}

func newProvisionerStore() *provisionerStore {
	clientset := fake.NewSimpleClientset()
	return &provisionerStore{SecretStore: NewK8sSecretStore(clientset), clientset: clientset}
}

func (s *provisionerStore) get(ref core.SecretRef) []byte {
	secret, err := s.clientset.CoreV1().Secrets(ref.Namespace).Get(context.Background(), ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return secret.Data[ref.Key]
}

func newTestProvisioner(cfg *core.Config, store core.SecretStore, buckets BucketCreator) *Provisioner {
	return newTestProvisionerAt(cfg, store, buckets, func() time.Time { return time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC) })
}

func newTestProvisionerAt(cfg *core.Config, store core.SecretStore, buckets BucketCreator, clock func() time.Time) *Provisioner {
	return NewProvisioner(cfg, ProvisionerDeps{Store: store, Buckets: buckets, Log: slog.Default(), Clock: clock})
}

// newTestProvisionerWith is newTestProvisioner with an in-process server.
func newTestProvisionerWith(cfg *core.Config, store core.SecretStore, worlds WorldsApplier) *Provisioner {
	return NewProvisioner(cfg, ProvisionerDeps{Store: store, Buckets: &brokertest.FakeBuckets{}, Worlds: worlds, Log: slog.Default()})
}

func TestTenantSlugShape(t *testing.T) {
	slug := TenantSlug("https://idp", "google|eve-123", "eve.adams@example.com")
	if !strings.HasPrefix(slug, "eve-adams-") {
		t.Errorf("slug = %q, want eve-adams- prefix", slug)
	}
	if len(strings.Split(slug, "-")) < 3 || len(slug) > 63 {
		t.Errorf("slug %q not a safe DNS label", slug)
	}
	// Stable across email display changes only via issuer|sub suffix:
	// same identity, same suffix.
	again := TenantSlug("https://idp", "google|eve-123", "eve.adams@example.com")
	if slug != again {
		t.Errorf("slug not deterministic: %q vs %q", slug, again)
	}
	// Different subject, different slug.
	other := TenantSlug("https://idp", "google|mallory", "eve.adams@example.com")
	if other == slug {
		t.Error("distinct subjects produced identical slugs")
	}
	// Hostile local parts sanitize to a DNS-safe label.
	weird := TenantSlug("https://idp", "s", "--Ünsafe..name!!@example.com")
	label, _, _ := strings.Cut(weird, "-")
	if label == "" || strings.HasPrefix(weird, "-") || strings.Contains(weird, "--") {
		t.Errorf("hostile local part produced unsafe slug %q", weird)
	}
}

// seedRegistry writes tenants as the registry document.
func seedRegistry(t *testing.T, cfg *core.Config, store core.SecretStore, tenants map[string]tenantRecord) {
	t.Helper()
	err := store.Mutate(context.Background(), core.RegistryRef(cfg), func([]byte) ([]byte, error) {
		return json.Marshal(tenantRegistry{Tenants: tenants})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// readRegistry decodes the registry document.
func readRegistry(t *testing.T, cfg *core.Config, store *provisionerStore) tenantRegistry {
	t.Helper()
	registry, err := decodeRegistry(store.get(core.RegistryRef(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// A slug another identity holds is that tenant's world and bucket: a newcomer
// whose short slug collides takes a wider one and overwrites nothing.
func TestEnsureTenantNeverTakesAnotherIdentitysSlug(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	eve := brokertest.EveClaims()
	short := TenantSlug(cfg.OIDC.Issuer, eve.Subject, core.CanonicalEmail(eve.Email))
	victim := tenantRecord{Email: "eve.adams@other.test", Issuer: cfg.OIDC.Issuer, Subject: "idp|victim", WorldID: "victim-world", Created: "2026-01-01T00:00:00Z"}
	seedRegistry(t, cfg, store, map[string]tenantRecord{short: victim})
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})

	world, err := p.EnsureTenant(context.Background(), eve)
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if world.Name == short || !strings.HasPrefix(world.Name, short) {
		t.Fatalf("world = %q, want a wider slug than the held %q", world.Name, short)
	}
	registry := readRegistry(t, cfg, store)
	if got := registry.Tenants[short]; got != victim {
		t.Fatalf("the held slug's record changed: %+v, want %+v", got, victim)
	}
	if got := registry.Tenants[world.Name]; got.Subject != eve.Subject {
		t.Fatalf("eve's record = %+v", got)
	}
	// The wider slug is pinned: the next arrival resolves it, not a third.
	again, err := p.EnsureTenant(context.Background(), eve)
	if err != nil || again.Name != world.Name {
		t.Fatalf("second arrival = %q, %v; want %q", again.Name, err, world.Name)
	}
}

// With every slug of the identity held by others, provisioning refuses.
func TestEnsureTenantRefusesWhenEverySlugIsTaken(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	eve := brokertest.EveClaims()
	held := map[string]tenantRecord{}
	for _, n := range slugSuffixBytes {
		slug := tenantSlug(cfg.OIDC.Issuer, eve.Subject, core.CanonicalEmail(eve.Email), n)
		held[slug] = tenantRecord{Issuer: cfg.OIDC.Issuer, Subject: "idp|holder-" + slug, WorldID: "w", Created: "2026-01-01T00:00:00Z"}
	}
	seedRegistry(t, cfg, store, held)
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})

	if _, err := p.EnsureTenant(context.Background(), eve); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("err = %v, want ErrSlugTaken", err)
	}
	if got := readRegistry(t, cfg, store); !maps.Equal(got.Tenants, held) {
		t.Fatalf("registry changed on a refusal: %+v", got.Tenants)
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	buckets := &brokertest.FakeBuckets{}
	p := newTestProvisioner(cfg, store, buckets)

	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if !strings.HasPrefix(world.Name, "eve-adams-") {
		t.Errorf("world name = %q", world.Name)
	}
	if !world.Local || world.InternalAddress != world.Name+".memory-worlds.svc.cluster.local:6309" {
		t.Errorf("world template wrong: %+v", world)
	}
	if len(world.Allow.Emails) != 1 || world.Allow.Emails[0] != "eve.adams@example.com" {
		t.Errorf("world allow = %+v, want the canonical email", world.Allow)
	}

	// Bucket created with prefix.
	if len(buckets.Created) != 1 || buckets.Created[0] != "memory-"+world.Name {
		t.Errorf("buckets created = %v", buckets.Created)
	}
	// Fragment rendered with the world, gated, and without the retired bootstrap key.
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	for _, want := range []string{"name: " + world.Name, "gated: true", "gs://memory-" + world.Name, world.Name + ".memory-worlds.svc.cluster.local"} {
		if !strings.Contains(fragment, want) {
			t.Errorf("fragment missing %q:\n%s", want, fragment)
		}
	}
	// Dynamic world visible to this pod's config immediately.
	if _, ok := p.registry.Find(world.Name); !ok {
		t.Error("provisioned world not in dynamic set")
	}

	// Second call: pure convergence, no new bucket entries beyond the
	// idempotent re-ensure, same world.
	again, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant (second): %v", err)
	}
	if again.Name != world.Name {
		t.Errorf("second EnsureTenant world = %q, want %q", again.Name, world.Name)
	}
	registry := string(store.get(core.RegistryRef(cfg)))
	if strings.Count(registry, `"email"`) != 1 {
		t.Errorf("registry has duplicate tenants:\n%s", registry)
	}
}

// movedEveClaims is eve after an email change touching both the local
// part and the domain: the recomputed slug would differ entirely.
func movedEveClaims() *core.Claims {
	return &core.Claims{Subject: "google|eve-123", Email: "Eve.Moved@elsewhere.io", EmailVerified: true}
}

// registryUpdateCount counts Secret update writes against the tenant
// registry, so tests can assert exactly when a refresh persists.
func registryUpdateCount(store *provisionerStore, cfg *core.Config) int {
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	original, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
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

	registry := string(store.get(core.RegistryRef(cfg)))
	if strings.Count(registry, `"email"`) != 1 {
		t.Errorf("email change duplicated the tenant:\n%s", registry)
	}
	if !strings.Contains(registry, "eve.moved@elsewhere.io") || strings.Contains(registry, "eve.adams@example.com") {
		t.Errorf("record email not refreshed:\n%s", registry)
	}
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	if strings.Count(fragment, "name: ") != 1 {
		t.Errorf("email change grew the fragment:\n%s", fragment)
	}
	// Dynamic world set converged on the new email for this pod.
	w, ok := p.registry.Find(original.Name)
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatal(err)
	}

	// Stale record: identity index hits but Allow rejects the new email,
	// denying so the slow path runs once and refreshes.
	if _, err := core.TenantWorldFor(p.registry.View(core.ProfileMemory), cfg.OIDC.Issuer, movedEveClaims()); !errors.Is(err, core.ErrNotAuthorized) {
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
	w, err := core.TenantWorldFor(p.registry.View(core.ProfileMemory), cfg.OIDC.Issuer, movedEveClaims())
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	eveWorld, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}
	twin := &core.Claims{Subject: "google|mallory", Email: "eve.adams@other.org", EmailVerified: true}
	twinWorld, err := p.EnsureTenant(context.Background(), twin)
	if err != nil {
		t.Fatal(err)
	}
	if twinWorld.Name == eveWorld.Name {
		t.Fatalf("shared local part collapsed two identities into %q", eveWorld.Name)
	}
	w, err := core.TenantWorldFor(p.registry.View(core.ProfileMemory), cfg.OIDC.Issuer, twin)
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	hooked := &hookedStore{SecretStore: raw, ref: core.RegistryRef(cfg)}
	p := newTestProvisioner(cfg, hooked, &brokertest.FakeBuckets{})
	sibling := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), raw, &brokertest.FakeBuckets{})

	// Right before p's registry mutate: the sibling provisions eve under
	// her original email, so p's create decision must become a hit.
	var originalWorld core.WorldConfig
	hooked.hook = func() {
		var hookErr error
		if originalWorld, hookErr = sibling.EnsureTenant(context.Background(), brokertest.EveClaims()); hookErr != nil {
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
	registry := string(raw.get(core.RegistryRef(cfg)))
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	failing := &hookedStore{SecretStore: raw, ref: core.WorldsFragmentRef(cfg)}
	p := newTestProvisioner(cfg, failing, &brokertest.FakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})

	seedRegistry := func(registry *tenantRegistry) {
		encoded, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Mutate(context.Background(), core.RegistryRef(cfg), func([]byte) ([]byte, error) {
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
	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if world.Name != "a-eve-0000" {
		t.Errorf("EnsureTenant resolved %q, want the first sorted slug a-eve-0000", world.Name)
	}
	w, err := core.TenantWorldFor(p.registry.View(core.ProfileMemory), cfg.OIDC.Issuer, brokertest.EveClaims())
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
	world, err = p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}
	if world.Name != "b-eve-1111" {
		t.Errorf("EnsureTenant with tombstoned duplicate resolved %q, want b-eve-1111", world.Name)
	}
	w, err = core.TenantWorldFor(p.registry.View(core.ProfileMemory), cfg.OIDC.Issuer, brokertest.EveClaims())
	if err != nil {
		t.Fatalf("tenantWorldFor with tombstoned duplicate: %v", err)
	}
	if w.Name != world.Name {
		t.Errorf("fast path resolved %q, EnsureTenant %q", w.Name, world.Name)
	}
}

func TestEnsureTenantGateModes(t *testing.T) {
	t.Run("static mode denies", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionStatic)
		p := newTestProvisioner(cfg, newProvisionerStore(), &brokertest.FakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
	})
	t.Run("allowlisted admits matching identity", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionAllowlisted)
		cfg.Provisioning.Allow = core.AllowConfig{Domains: []string{"example.com"}}
		p := newTestProvisioner(cfg, newProvisionerStore(), &brokertest.FakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
			t.Fatalf("EnsureTenant: %v", err)
		}
	})
	t.Run("allowlisted denies others", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionAllowlisted)
		cfg.Provisioning.Allow = core.AllowConfig{Domains: []string{"other.org"}}
		p := newTestProvisioner(cfg, newProvisionerStore(), &brokertest.FakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
	})
	t.Run("existing tenant bypasses a tightened gate", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
		store := newProvisionerStore()
		p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		cfg.Provisioning.Mode = core.ProvisionAllowlisted
		cfg.Provisioning.Allow = core.AllowConfig{Domains: []string{"other.org"}}
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
			t.Fatalf("existing tenant locked out by tightened gate: %v", err)
		}
	})
}

func TestEnsureTenantCapacity(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	cfg.Provisioning.MaxTenants = 1
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatalf("first tenant: %v", err)
	}
	second := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	if _, err := p.EnsureTenant(context.Background(), second); !errors.Is(err, ErrTenantCapacity) {
		t.Fatalf("err = %v, want ErrTenantCapacity", err)
	}
	// The existing tenant still converges at capacity.
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatalf("existing tenant blocked at capacity: %v", err)
	}
}

func TestEnsureTenantBucketFailureRetries(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	buckets := &brokertest.FakeBuckets{Fail: true}
	p := newTestProvisioner(cfg, store, buckets)
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err == nil {
		t.Fatal("EnsureTenant must surface bucket failure")
	}
	// Registry already holds the tenant; a retry converges the rest.
	buckets.Fail = false
	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatalf("retry after bucket failure: %v", err)
	}
	if len(store.get(core.WorldsFragmentRef(cfg))) == 0 {
		t.Error("fragment missing after converged retry")
	}
	if _, ok := p.registry.Find(world.Name); !ok {
		t.Error("world not visible after converged retry")
	}
}

func TestSyncRegistryPublishesDynamicWorlds(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatal(err)
	}

	// A sibling replica sees the tenant purely via registry sync.
	sibling := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	q := newTestProvisioner(sibling, store, &brokertest.FakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	slug := TenantSlug(cfg.OIDC.Issuer, "google|eve-123", "eve.adams@example.com")
	if _, ok := q.registry.Find(slug); !ok {
		t.Errorf("sibling did not converge on tenant %q", slug)
	}
}

func TestFragmentRendersDocumentQuota(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	cfg.Provisioning.MaxDocumentsPerTenant = 500
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatal(err)
	}
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	if !strings.Contains(fragment, "maxDocuments: 500") {
		t.Errorf("fragment missing per-tenant quota:\n%s", fragment)
	}
}

// TestWorldsFragmentGolden pins the rendered fragment as the
// producer-consumer contract fixture the server's knowledgeconfig test
// parses; a shape drift breaks one side visibly.
func TestWorldsFragmentGolden(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	cfg.Provisioning.MaxDocumentsPerTenant = 500
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatal(err)
	}
	rendered := store.get(core.WorldsFragmentRef(cfg))

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

// The fragment is rendered from the registry as it stands at the write, so a
// tenant a sibling provisioned after this pod read the registry is kept.
func TestFragmentKeepsATenantProvisionedSinceTheRegistryRead(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	hooked := &hookedStore{SecretStore: raw, ref: core.WorldsFragmentRef(cfg)}
	p := newTestProvisioner(cfg, hooked, &brokertest.FakeBuckets{})
	sibling := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), raw, &brokertest.FakeBuckets{})

	frank := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	var frankWorld core.WorldConfig
	hooked.hook = func() {
		var hookErr error
		if frankWorld, hookErr = sibling.EnsureTenant(context.Background(), frank); hookErr != nil {
			t.Errorf("racing EnsureTenant(frank): %v", hookErr)
		}
	}
	eveWorld, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}
	doc, unknown, err := worldsfragment.Parse(raw.get(core.WorldsFragmentRef(cfg)))
	if err != nil || unknown != "" {
		t.Fatalf("parse fragment: %q, %v", unknown, err)
	}
	got := make([]string, 0, len(doc.Worlds))
	for i := range doc.Worlds {
		got = append(got, doc.Worlds[i].Name)
	}
	// Exact sorted set: no drops, no duplicates, and the deterministic
	// order Mutate's unchanged-bytes suppression depends on.
	want := []string{eveWorld.Name, frankWorld.Name}
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("fragment worlds = %v, want %v", got, want)
	}
	if _, ok := p.registry.Find(frankWorld.Name); !ok {
		t.Error("this pod's world set lacks the tenant its own fragment names")
	}
}

// A tenant deprovisioned between this pod's registry read and its fragment
// write must stay gone: once its record is cleared nothing would ever take
// a resurrected world out of the fragment again.
func TestStaleRegistryReadCannotResurrectADeprovisionedWorld(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	operator := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), raw, &brokertest.FakeBuckets{})
	frank := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	frankWorld, err := operator.EnsureTenant(context.Background(), frank)
	if err != nil {
		t.Fatal(err)
	}

	// Eve arrives on a serving pod: after its registry write (frank still
	// live) and before its fragment write, the operator deprovisions frank
	// start to finish.
	hooked := &hookedStore{SecretStore: raw, ref: core.WorldsFragmentRef(cfg)}
	p := newTestProvisioner(cfg, hooked, &brokertest.FakeBuckets{})
	hooked.hook = func() {
		if _, hookErr := operator.DeprovisionTenant(context.Background(), frankWorld.Name, false); hookErr != nil {
			t.Errorf("racing DeprovisionTenant: %v", hookErr)
		}
	}
	eveWorld, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}

	fragment := string(raw.get(core.WorldsFragmentRef(cfg)))
	if strings.Contains(fragment, "name: "+frankWorld.Name) {
		t.Errorf("a deprovisioned world came back into the fragment:\n%s", fragment)
	}
	if !strings.Contains(fragment, "name: "+eveWorld.Name) {
		t.Errorf("the new tenant is missing from the fragment:\n%s", fragment)
	}
	if _, ok := p.registry.Find(frankWorld.Name); ok {
		t.Error("a deprovisioned world came back into this pod's world set")
	}
	if _, held := readRegistry(t, cfg, raw).Tenants[frankWorld.Name]; held {
		t.Error("the deprovisioned record is back in the registry")
	}
}

// hookedStore fires hook once, right before the first Mutate of ref,
// to interleave a concurrent actor at an exact point in a flow.
type hookedStore struct {
	core.SecretStore
	mu   sync.Mutex
	ref  core.SecretRef
	hook func()
}

func (h *hookedStore) Mutate(ctx context.Context, ref core.SecretRef, fn func([]byte) ([]byte, error)) error {
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
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	hooked := &hookedStore{SecretStore: raw, ref: core.WorldsFragmentRef(cfg)}
	p := newTestProvisioner(cfg, hooked, &brokertest.FakeBuckets{})
	eveWorld, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}

	// Between p's registry tombstone and its fragment rewrite: a sibling
	// provisions frank (postdating p's snapshot) and eve tries to come
	// back (must be blocked by the tombstone).
	sibling := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), raw, &brokertest.FakeBuckets{})
	frank := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
	var frankWorld core.WorldConfig
	hooked.hook = func() {
		var hookErr error
		if frankWorld, hookErr = sibling.EnsureTenant(context.Background(), frank); hookErr != nil {
			t.Errorf("racing EnsureTenant(frank): %v", hookErr)
		}
		if _, hookErr = sibling.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(hookErr, ErrTenantDeprovisioning) {
			t.Errorf("EnsureTenant on tombstoned slug err = %v, want ErrTenantDeprovisioning", hookErr)
		}
	}

	if _, err := p.DeprovisionTenant(context.Background(), eveWorld.Name, false); err != nil {
		t.Fatalf("DeprovisionTenant: %v", err)
	}
	fragment := string(raw.get(core.WorldsFragmentRef(cfg)))
	if strings.Contains(fragment, "name: "+eveWorld.Name) {
		t.Errorf("deprovisioned world survived the fragment rewrite:\n%s", fragment)
	}
	if !strings.Contains(fragment, "name: "+frankWorld.Name) {
		t.Errorf("racing sibling world dropped by deprovision:\n%s", fragment)
	}
	// Tombstone cleared: eve can provision again.
	if _, err := sibling.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Errorf("re-provision after completed deprovision: %v", err)
	}
}

// TestSyncRemovesTombstonedWorld: a crashed deprovision leaves the
// tombstone; the sync loop must still take the world out of service.
func TestSyncRemovesTombstonedWorld(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	raw := newProvisionerStore()
	// Crash simulation: fail the tombstone's fragment rewrite so
	// DeprovisionTenant exits after phase 1.
	failing := &hookedStore{SecretStore: raw, ref: core.WorldsFragmentRef(cfg)}
	p := newTestProvisioner(cfg, failing, &brokertest.FakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
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
	sibling := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	q := newTestProvisioner(sibling, raw, &brokertest.FakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	fragment := string(raw.get(core.WorldsFragmentRef(cfg)))
	if strings.Contains(fragment, "name: "+world.Name) {
		t.Errorf("sync kept a tombstoned world in the fragment:\n%s", fragment)
	}
	if _, ok := q.registry.Find(world.Name); ok {
		t.Error("sync kept a tombstoned world resolvable")
	}
}

var errAbortMutate = errors.New("test: abort mutate")

// TestSyncRegistryConvergesFragment: the sync loop repairs a fragment
// that lost a registry-held world to a stale rewrite.
func TestSyncRegistryConvergesFragment(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the stale drop: empty the fragment while the registry
	// still holds the tenant.
	if err := p.store.Mutate(context.Background(), core.WorldsFragmentRef(cfg), func([]byte) ([]byte, error) {
		return []byte("worlds: []\n"), nil
	}); err != nil {
		t.Fatal(err)
	}

	sibling := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	q := newTestProvisioner(sibling, store, &brokertest.FakeBuckets{})
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	if !strings.Contains(fragment, "name: "+world.Name) {
		t.Errorf("sync did not restore the dropped world:\n%s", fragment)
	}
}

func TestDeprovisionTenantRemovesEverything(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	buckets := &brokertest.FakeBuckets{}
	p := newTestProvisioner(cfg, store, buckets)

	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatal(err)
	}
	frank := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
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
	registry := string(store.get(core.RegistryRef(cfg)))
	if strings.Contains(registry, world.Name) || !strings.Contains(registry, frankWorld.Name) {
		t.Errorf("registry after deprovision:\n%s", registry)
	}
	// Fragment rewrite dropped eve's world (the one flow allowed to).
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	if strings.Contains(fragment, "name: "+world.Name) || !strings.Contains(fragment, "name: "+frankWorld.Name) {
		t.Errorf("fragment after deprovision:\n%s", fragment)
	}
	// Bucket deleted on request.
	deleted := buckets.DeletedBuckets()
	if len(deleted) != 1 || deleted[0] != "memory-"+world.Name {
		t.Errorf("deleted buckets = %v", deleted)
	}
	// Dynamic world set no longer resolves eve.
	if _, ok := p.registry.Find(world.Name); ok {
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

// A provisioned world is addressed like a configured one, so its name obeys
// the same rule, whatever the email looks like.
func TestTenantSlugIsAlwaysAValidWorldName(t *testing.T) {
	for _, email := range []string{"Eve.Adams+x@example.com", "___@example.com", "", "-lead-@example.com", strings.Repeat("a", 80) + "@example.com", "日本@example.com"} {
		if slug := TenantSlug("https://issuer", "sub", email); !protocol.IsWorldName(slug) {
			t.Errorf("tenantSlug(%q) = %q, not a valid world name", email, slug)
		}
	}
}

// A refusal the gate decided is answered from memory until the TTL passes,
// so a refused identity's calls do not queue on the provisioner's lock.
func TestEnsureTenantRemembersRefusal(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionStatic)
	clk := brokertest.NewFakeClock(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC))
	p := newTestProvisionerAt(cfg, newProvisionerStore(), &brokertest.FakeBuckets{}, clk.Now)
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
		t.Fatalf("err = %v, want ErrProvisioningDenied", err)
	}
	cfg.Provisioning.Mode = core.ProvisionOpen
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
		t.Fatalf("within the TTL err = %v, want the remembered refusal", err)
	}
	clk.Advance(refusalTTL)
	if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatalf("after the TTL: %v, want the gate consulted again", err)
	}
}

// A registry change may admit a refused identity, so a finished deprovision
// and a sync that saw a change both clear the memory at once.
func TestRegistryChangesClearRememberedRefusals(t *testing.T) {
	bob := &core.Claims{Subject: "google|bob-1", Email: "bob@example.com", EmailVerified: true}
	t.Run("deprovision frees capacity", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
		cfg.Provisioning.MaxTenants = 1
		store := newProvisionerStore()
		p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
		eve, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
		if err != nil {
			t.Fatalf("provision eve: %v", err)
		}
		if _, err := p.EnsureTenant(context.Background(), bob); !errors.Is(err, ErrTenantCapacity) {
			t.Fatalf("bob err = %v, want ErrTenantCapacity", err)
		}
		if _, err := p.DeprovisionTenant(context.Background(), eve.Name, false); err != nil {
			t.Fatalf("deprovision eve: %v", err)
		}
		if _, err := p.EnsureTenant(context.Background(), bob); err != nil {
			t.Fatalf("bob after deprovision: %v, want admitted at once", err)
		}
	})
	t.Run("sync during the refusal cannot be outrun", func(t *testing.T) {
		// Order: p refuses eve under its lock; before that refusal is
		// remembered, a sibling provisions eve and p's sync clears the
		// memory. The refusal must be remembered before the sync runs.
		cfg := brokertest.NewProvisioningConfig(core.ProvisionStatic)
		store := newProvisionerStore()
		sibling := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), store, &brokertest.FakeBuckets{})
		var p *Provisioner
		calls := 0
		synced := make(chan error, 1)
		clock := func() time.Time {
			calls++
			// Call 1 checks the memory, call 2 remembers the refusal.
			if calls != 2 {
				return time.Now()
			}
			if _, err := sibling.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
				t.Errorf("sibling provisions eve: %v", err)
			}
			go func() { synced <- p.SyncRegistry(context.Background()) }()
			select {
			case err := <-synced:
				synced <- err // the sync outran the memory; the next call sees the stale refusal
			case <-time.After(100 * time.Millisecond):
			}
			return time.Now()
		}
		p = newTestProvisionerAt(cfg, store, &brokertest.FakeBuckets{}, clock)
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
		if err := <-synced; err != nil {
			t.Fatalf("SyncRegistry: %v", err)
		}
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
			t.Fatalf("eve after the sync: %v, want the sibling's record honoured", err)
		}
	})
	t.Run("sync sees a sibling's change", func(t *testing.T) {
		cfg := brokertest.NewProvisioningConfig(core.ProvisionStatic)
		store := newProvisionerStore()
		p := newTestProvisioner(cfg, store, &brokertest.FakeBuckets{})
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); !errors.Is(err, ErrProvisioningDenied) {
			t.Fatalf("err = %v, want ErrProvisioningDenied", err)
		}
		cfg.Provisioning.Mode = core.ProvisionOpen
		sibling := newTestProvisioner(brokertest.NewProvisioningConfig(core.ProvisionOpen), store, &brokertest.FakeBuckets{})
		if _, err := sibling.EnsureTenant(context.Background(), bob); err != nil {
			t.Fatalf("sibling provisions bob: %v", err)
		}
		if err := p.SyncRegistry(context.Background()); err != nil {
			t.Fatalf("SyncRegistry: %v", err)
		}
		if _, err := p.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
			t.Fatalf("eve after the sync: %v, want the gate consulted again", err)
		}
	})
}

// fakeWorlds records every fragment pushed to the in-process server.
type fakeWorlds struct {
	fragments []string
	fail      bool
}

func (f *fakeWorlds) ApplyWorldsFragment(fragment []byte) error {
	if f.fail {
		return errors.New("server refused the fragment")
	}
	f.fragments = append(f.fragments, string(fragment))
	return nil
}

// blockingWorlds parks every push until release closes, as a server opening
// a slow world would.
type blockingWorlds struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingWorlds) ApplyWorldsFragment([]byte) error {
	b.entered <- struct{}{}
	<-b.release
	return nil
}

// The in-process push opens worlds, so it runs outside the provisioner's
// lock: a slow open must not stall the registry sync or other identities.
func TestProvisionerPushRunsOutsideItsLock(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	worlds := &blockingWorlds{entered: make(chan struct{}, 1), release: make(chan struct{})}
	p := newTestProvisionerWith(cfg, newProvisionerStore(), worlds)
	done := make(chan error, 1)
	go func() {
		_, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
		done <- err
	}()
	<-worlds.entered
	if !p.projectMu.TryLock() || !p.mu.TryLock() {
		close(worlds.release)
		t.Fatal("a provisioner lock is held across the in-process push")
	}
	p.mu.Unlock()
	p.projectMu.Unlock()
	close(worlds.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A push that lost the race to a newer fragment must not put the older
// world set back in force.
func TestProvisionerNeverPushesAnOlderFragmentOverANewer(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	worlds := &fakeWorlds{}
	p := newTestProvisionerWith(cfg, newProvisionerStore(), worlds)
	older := renderedFragment{body: []byte("older"), seq: 1}
	newer := renderedFragment{body: []byte("newer"), seq: 2}
	if !p.pushWorlds(newer) || !p.pushWorlds(older) {
		t.Fatal("a push the server already covers must report it holds the fragment")
	}
	if !slices.Equal(worlds.fragments, []string{"newer"}) {
		t.Fatalf("pushed %q, want only the newer fragment", worlds.fragments)
	}
}

// TestProvisionerPushesWorldsInProcess: every registry change reaches the
// server in this process at once, and a refused push never fails the flow.
func TestProvisionerPushesWorldsInProcess(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	worlds := &fakeWorlds{}
	p := newTestProvisionerWith(cfg, store, worlds)

	world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
	if err != nil {
		t.Fatalf("EnsureTenant: %v", err)
	}
	if len(worlds.fragments) != 1 || !strings.Contains(worlds.fragments[0], "name: "+world.Name) {
		t.Fatalf("pushed fragments after provisioning = %q, want one naming %s", worlds.fragments, world.Name)
	}
	if got := string(store.get(core.WorldsFragmentRef(cfg))); got != worlds.fragments[0] {
		t.Errorf("pushed fragment differs from the durable one:\n%s\n---\n%s", worlds.fragments[0], got)
	}

	if _, err := p.DeprovisionTenant(context.Background(), world.Name, false); err != nil {
		t.Fatalf("DeprovisionTenant: %v", err)
	}
	if last := worlds.fragments[len(worlds.fragments)-1]; strings.Contains(last, world.Name) {
		t.Errorf("deprovision push still names the world:\n%s", last)
	}

	// A sibling with a refusing server keeps converging through the Secret.
	sibling := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	q := newTestProvisionerWith(sibling, store, &fakeWorlds{fail: true})
	if _, err := q.EnsureTenant(context.Background(), brokertest.EveClaims()); err != nil {
		t.Fatalf("EnsureTenant with a refusing server: %v", err)
	}
	if err := q.SyncRegistry(context.Background()); err != nil {
		t.Fatalf("SyncRegistry with a refusing server: %v", err)
	}
	if q.lastSync != [32]byte{} {
		t.Fatal("a refused push marked the registry payload synced; the next tick would not retry it")
	}
}

// blockingBuckets parks EnsureBucket for the buckets named in park until
// release closes; every other bucket is created at once.
type blockingBuckets struct {
	brokertest.FakeBuckets
	park    string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBuckets) EnsureBucket(ctx context.Context, bucket string) error {
	if strings.Contains(bucket, b.park) {
		b.entered <- struct{}{}
		<-b.release
	}
	return b.FakeBuckets.EnsureBucket(ctx, bucket)
}

// One tenant's slow bucket is that tenant's alone: another identity's first
// arrival completes while it hangs, and so does the registry sync.
func TestSlowBucketHoldsUpOnlyItsOwnTenant(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	store := newProvisionerStore()
	buckets := &blockingBuckets{park: "eve-adams", entered: make(chan struct{}, 4), release: make(chan struct{})}
	p := newTestProvisioner(cfg, store, buckets)

	slow := make(chan error, 1)
	go func() {
		_, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
		slow <- err
	}()
	<-buckets.entered

	done := make(chan error, 1)
	go func() {
		frank := &core.Claims{Subject: "google|frank", Email: "frank@example.com", EmailVerified: true}
		world, err := p.EnsureTenant(context.Background(), frank)
		if err == nil {
			if _, ok := p.registry.Find(world.Name); !ok {
				err = errors.New("frank's world is not in the world set")
			}
		}
		if err == nil {
			err = p.SyncRegistry(context.Background())
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another identity's provisioning waited on a tenant's slow bucket")
	}

	close(buckets.release)
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	fragment := string(store.get(core.WorldsFragmentRef(cfg)))
	if !strings.Contains(fragment, "name: eve-adams-") || !strings.Contains(fragment, "name: frank-") {
		t.Fatalf("fragment lacks one of the two tenants:\n%s", fragment)
	}
}

// Concurrent arrivals of one identity are one flight: one bucket call, one
// world, and a caller that leaves does not fail the ones still waiting.
func TestConcurrentArrivalsOfOneIdentityShareAFlight(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	buckets := &blockingBuckets{park: "eve-adams", entered: make(chan struct{}, 4), release: make(chan struct{})}
	p := newTestProvisioner(cfg, newProvisionerStore(), buckets)

	leaving, leave := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, err := p.EnsureTenant(leaving, brokertest.EveClaims())
		left <- err
	}()
	<-buckets.entered

	worlds := make(chan string, 3)
	calling := make(chan struct{}, 3)
	for range 3 {
		go func() {
			calling <- struct{}{}
			world, err := p.EnsureTenant(context.Background(), brokertest.EveClaims())
			if err != nil {
				t.Errorf("EnsureTenant: %v", err)
			}
			worlds <- world.Name
		}()
	}
	for range 3 {
		<-calling
	}
	// Let the three reach the flight before it is released; a flight has no
	// way to say who waits on it.
	time.Sleep(200 * time.Millisecond)
	leave()
	if err := <-left; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that left: err = %v, want its own cancellation", err)
	}
	close(buckets.release)
	first := <-worlds
	for range 2 {
		if got := <-worlds; got != first || got == "" {
			t.Fatalf("callers of one flight got worlds %q and %q", first, got)
		}
	}
	select {
	case <-buckets.entered:
		t.Fatal("one identity's concurrent arrivals reached the bucket more than once")
	default:
	}
}

// A refusal decided on a registry this pod has since moved past is not
// remembered: the newer registry may admit the identity.
func TestRefusalDecidedBeforeANewerRegistryIsNotRemembered(t *testing.T) {
	cfg := brokertest.NewProvisioningConfig(core.ProvisionOpen)
	cfg.Provisioning.MaxTenants = 1
	raw := newProvisionerStore()
	seedRegistry(t, cfg, raw, map[string]tenantRecord{
		"frank-00000000": {Email: "frank@example.com", Issuer: cfg.OIDC.Issuer, Subject: "google|frank", WorldID: "w", Created: "2026-01-01T00:00:00Z"},
	})
	hooked := &hookedStore{SecretStore: raw, ref: core.RegistryRef(cfg)}
	p := newTestProvisioner(cfg, hooked, &brokertest.FakeBuckets{})
	// Between the flight's start and its registry write, a sync applies.
	hooked.hook = func() {
		if err := p.SyncRegistry(context.Background()); err != nil {
			t.Errorf("SyncRegistry: %v", err)
		}
	}
	eve := brokertest.EveClaims()
	if _, err := p.EnsureTenant(context.Background(), eve); !errors.Is(err, ErrTenantCapacity) {
		t.Fatalf("err = %v, want ErrTenantCapacity", err)
	}
	if held := p.refusals.recent(core.IdentityKey(cfg.OIDC.Issuer, eve.Subject), p.clock()); held != nil {
		t.Fatalf("a refusal older than the applied registry was remembered: %v", held)
	}
	// Decided on the registry in force, it is.
	if _, err := p.EnsureTenant(context.Background(), eve); !errors.Is(err, ErrTenantCapacity) {
		t.Fatalf("err = %v, want ErrTenantCapacity", err)
	}
	if held := p.refusals.recent(core.IdentityKey(cfg.OIDC.Issuer, eve.Subject), p.clock()); held == nil {
		t.Fatal("a refusal on the registry in force was not remembered")
	}
}
