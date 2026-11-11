package core

import "testing"

// The broker's private worlds are its local memory worlds and every tenant
// the provisioner can create; nothing else must be gated on its account.
func TestMustGateNamesThePrivateWorlds(t *testing.T) {
	cfg := &Config{
		Worlds: []WorldConfig{
			{Name: "team", Profile: ProfileKnowledge, Local: true, InternalAddress: "team.example.com:6309"},
			{Name: "alice", Profile: ProfileMemory, Local: true, InternalAddress: "Alice.Example.com:6309"},
			{Name: "remote", Profile: ProfileMemory, InternalAddress: "remote.example.com:6309"},
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
		"tenants.example.com":              false,
		"eviltenants.example.com":          false,
		"*":                                false,
	} {
		if got := must(authority); got != want {
			t.Errorf("must(%q) = %v, want %v", authority, got, want)
		}
	}

	cfg.Provisioning.Mode = ProvisionStatic
	if cfg.MustGate()("eve-1a2b3c4d.tenants.example.com") {
		t.Error("a tenant authority must be gated although provisioning is off")
	}
}
