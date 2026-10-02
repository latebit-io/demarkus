package core

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
)

// Provisioning-gate modes (memory-broker plan Phase 3): whether a first
// authenticated arrival with no world gets one created.
const (
	ProvisionStatic      = "static"
	ProvisionAllowlisted = "allowlisted"
	ProvisionOpen        = "open"
)

// ProvisioningConfig is the memory broker's dynamic-tenant gate and
// world template: static disables provisioning, allowlisted gates on
// Allow, open admits anyone and requires MaxTenants as the cap.
type ProvisioningConfig struct {
	Mode string `yaml:"mode"`
	// Allow gates allowlisted mode: domains, groups, and emails use the
	// same predicate as per-world allowlists.
	Allow AllowConfig `yaml:"allow"`
	// MaxTenants caps the dynamic tenant count. 0 means uncapped
	// (rejected in open mode).
	MaxTenants int `yaml:"maxTenants"`
	// AuthorityDomain is the DNS suffix for tenant authorities
	// (<slug>.<authorityDomain>); must match the server's SNI routing.
	AuthorityDomain string `yaml:"authorityDomain"`
	// BucketPrefix prefixes each tenant's GCS bucket: gs://<prefix><slug>.
	BucketPrefix string `yaml:"bucketPrefix"`
	// BucketProject is the GCP project buckets are created in.
	BucketProject string `yaml:"bucketProject"`
	// BucketLocation is the GCS location for new buckets (e.g. EU, US,
	// europe-west1). Blank uses the GCS default (US multi-region);
	// set it explicitly when data residency matters.
	BucketLocation string `yaml:"bucketLocation"`
	// WorldsSecret names the Secret whose worlds.yaml key carries the
	// knowledge-server worlds fragment (the server mounts and watches it).
	WorldsSecret string `yaml:"worldsSecret"`
	// RegistrySecret names the broker-namespace Secret holding the
	// tenant registry (registry.json). Defaulted when blank.
	RegistrySecret string `yaml:"registrySecret"`
	// MaxDocumentsPerTenant caps distinct document paths in each tenant
	// world (rendered into the server fragment as limits.maxDocuments).
	// 0 = unlimited.
	MaxDocumentsPerTenant int `yaml:"maxDocumentsPerTenant"`
}

const defaultRegistrySecret = "demarkus-memory-broker-registry"

// Enabled reports whether dynamic provisioning is on.
func (p *ProvisioningConfig) Enabled() bool {
	return p.Mode == ProvisionAllowlisted || p.Mode == ProvisionOpen
}

// validate normalizes and checks the provisioning block; the caller
// requires the memory gateway when it is enabled.
func (p *ProvisioningConfig) validate() error {
	p.Mode = strings.TrimSpace(p.Mode)
	switch p.Mode {
	case "", ProvisionStatic:
		p.Mode = ProvisionStatic
		return nil
	case ProvisionAllowlisted, ProvisionOpen:
	default:
		return fmt.Errorf("provisioning.mode must be %q, %q, or %q (got %q)", ProvisionStatic, ProvisionAllowlisted, ProvisionOpen, p.Mode)
	}
	if p.Mode == ProvisionOpen && p.MaxTenants <= 0 {
		return fmt.Errorf("provisioning.maxTenants must be positive in open mode (unbounded open signup is a resource-creation DoS surface)")
	}
	required := map[string]string{
		"provisioning.authorityDomain": p.AuthorityDomain,
		"provisioning.bucketPrefix":    p.BucketPrefix,
		"provisioning.bucketProject":   p.BucketProject,
		"provisioning.worldsSecret":    p.WorldsSecret,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required when provisioning is enabled", field)
		}
	}
	if p.RegistrySecret == "" {
		p.RegistrySecret = defaultRegistrySecret
	}
	return nil
}

// tenantAuthority renders a tenant world's logical authority host:port.
func (p *ProvisioningConfig) tenantAuthority(slug string) string {
	return fmt.Sprintf("%s.%s:%d", slug, p.AuthorityDomain, protocol.DefaultPort)
}

// TenantWorld renders the broker-side WorldConfig for one tenant. A tenant
// is served by the server in this process, so it is local, addressed by its
// authority, and written only under the identity grant.
func (p *ProvisioningConfig) TenantWorld(slug, email string) WorldConfig {
	return WorldConfig{
		Name:            slug,
		Profile:         ProfileMemory,
		InternalAddress: p.tenantAuthority(slug),
		Local:           true,
		Allow:           AllowConfig{Emails: []string{email}},
		WriteScope:      WriteScope{Paths: []string{"/**"}},
	}
}
