package core

import "testing"

// The broker's private worlds are its local memory worlds and every authority
// under the tenant domain; nothing else must be gated on its account.
func TestMustGateNamesThePrivateWorlds(t *testing.T) {
	cfg := &Config{
		Worlds: []WorldConfig{
			{Name: "team", Profile: ProfileKnowledge, Local: true, InternalAddress: "team.example.com:6309"},
			{Name: "alice", Profile: ProfileMemory, Local: true, InternalAddress: "Alice.Example.com:6309"},
			{Name: "remote", Profile: ProfileMemory, InternalAddress: "remote.example.com:6309"},
			{Name: "hub", Profile: ProfileKnowledge, Local: true, InternalAddress: "hub.tenants.example.com:6309"},
		},
		Provisioning: ProvisioningConfig{Mode: ProvisionOpen, AuthorityDomain: "Tenants.example.com"},
	}
	must := cfg.MustGate()
	for authority, want := range map[string]bool{
		"alice.example.com":                true,
		"ALICE.example.com":                true,
		"eve-1a2b3c4d.tenants.example.com": true,
		"team.example.com":                 false,
		"remote.example.com":               false, // served elsewhere, not this server's to gate
		"hub.tenants.example.com":          false, // the chart puts knowledge worlds under the tenant domain
		"tenants.example.com":              false,
		"eviltenants.example.com":          false,
		"*":                                false,
	} {
		if got := must(authority); got != want {
			t.Errorf("must(%q) = %v, want %v", authority, got, want)
		}
	}

	// Provisioning off still serves the tenants a mounted fragment names.
	cfg.Provisioning.Mode = ProvisionStatic
	if !cfg.MustGate()("eve-1a2b3c4d.tenants.example.com") {
		t.Error("turning provisioning off ungated a tenant authority")
	}
	cfg.Provisioning.AuthorityDomain = ""
	if cfg.MustGate()("eve-1a2b3c4d.tenants.example.com") {
		t.Error("an authority is gated as a tenant with no tenant domain configured")
	}
}
