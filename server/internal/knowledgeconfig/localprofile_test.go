package knowledgeconfig

import (
	"strings"
	"testing"
)

// The local profile as YAML would spell it: one file world on every server
// name, no certificate pair, no health port, no policy seed.
const localProfile = `version: 1
health:
  address: ""
worlds:
  - name: soul
    authorities: ["*"]
    dir: /var/lib/demarkus/soul
    policy:
      optional: true
`

func TestParseLocalProfile(t *testing.T) {
	config, err := Parse([]byte(localProfile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	world := config.Worlds[0]
	if world.Authorities[0] != AnyAuthority || !world.Policy.Optional || config.Health.Address != "" || config.TLS.CertFile != "" {
		t.Errorf("local profile = authorities %v, optional %t, health %q, cert %q", world.Authorities, world.Policy.Optional, config.Health.Address, config.TLS.CertFile)
	}
	if defaults, err := Parse([]byte(validConfig)); err != nil || defaults.Worlds[0].Policy.Optional || defaults.Health.Address != ":8081" {
		t.Errorf("defaults moved: err %v, optional %t, health %q", err, defaults.Worlds[0].Policy.Optional, defaults.Health.Address)
	}
	// The constructor the thin main uses parses to the same world once
	// defaults are filled.
	built := LocalProfile(&LocalOptions{Dir: "/var/lib/demarkus/soul"})
	built.SetDefaults()
	if err := built.Validate(); err != nil {
		t.Fatalf("LocalProfile does not validate: %v", err)
	}
	if built.Worlds[0].Limits.MaxConcurrentRequests != defaultMaxConcurrentRequests || built.Worlds[0].Limits.MaxWatches != defaultMaxWatches || built.Worlds[0].Policy.Path != defaultPolicyPath {
		t.Errorf("LocalProfile defaults = %+v, policy %q", built.Worlds[0].Limits, built.Worlds[0].Policy.Path)
	}
}

func TestValidateLocalProfile(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"star beside another authority", strings.Replace(localProfile, `authorities: ["*"]`, `authorities: ["*", "soul.example.com"]`, 1), `"*" serves every server name, so it must be the only authority of the only world`},
		{"star with a second world", localProfile + "  - name: other\n    authorities: [other.example.com]\n    dir: /var/lib/demarkus/other\n", `"*" serves every server name, so it must be the only authority of the only world`},
		{"one TLS file", localProfile + "tls:\n  certFile: /etc/demarkus/tls.crt\n", "tls.certFile and tls.keyFile are set together"},
		{"policy file without a seed", strings.Replace(localProfile, "      optional: true\n", "      optional: true\n      file: /etc/demarkus/policy.md\n", 1), "policy.file must not be set on an optional policy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
