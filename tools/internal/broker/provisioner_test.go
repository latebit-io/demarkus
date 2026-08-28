package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
