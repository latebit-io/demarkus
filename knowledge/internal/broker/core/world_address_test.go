package core

import "testing"

func TestWorldAddressAndAuthority(t *testing.T) {
	tests := []struct {
		name                       string
		world                      WorldConfig
		wantAddress, wantAuthority string
	}{
		{
			name:          "Service DNS in the world's namespace",
			world:         WorldConfig{Name: "team-a", Namespace: "team-a"},
			wantAddress:   "team-a.team-a.svc.cluster.local:6309",
			wantAuthority: "team-a.team-a.svc.cluster.local",
		},
		{
			name:          "Service DNS in a shared namespace",
			world:         WorldConfig{Name: "team-b", Namespace: "shared-worlds"},
			wantAddress:   "team-b.shared-worlds.svc.cluster.local:6309",
			wantAuthority: "team-b.shared-worlds.svc.cluster.local",
		},
		{
			name:          "InternalAddress wins",
			world:         WorldConfig{Name: "team-c", Namespace: "team-c", InternalAddress: "team-c-mark.platform:7000"},
			wantAddress:   "team-c-mark.platform:7000",
			wantAuthority: "team-c-mark.platform",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.world.Address(); got != tt.wantAddress {
				t.Errorf("Address = %q, want %q", got, tt.wantAddress)
			}
			if got := tt.world.Authority(); got != tt.wantAuthority {
				t.Errorf("Authority = %q, want %q", got, tt.wantAuthority)
			}
		})
	}
}
