package core

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigFederation(t *testing.T) {
	clearConfigEnv(t)
	local := validConfig + "    local: true\n"
	tests := []struct {
		name    string
		body    string
		wantErr string
		want    FederationConfig
	}{
		{name: "off without a hub", body: local},
		{
			name: "defaults",
			body: local + "federation:\n  hub: team-a\n",
			want: FederationConfig{Hub: "team-a", LeaseName: "demarkus-federation", QuietPeriod: 30 * time.Second, Interval: time.Minute},
		},
		{
			name: "explicit",
			body: local + "federation:\n  hub: team-a\n  leaseName: fed\n  quietPeriod: 1s\n  interval: 2s\n",
			want: FederationConfig{Hub: "team-a", LeaseName: "fed", QuietPeriod: time.Second, Interval: 2 * time.Second},
		},
		{name: "unknown hub", body: local + "federation:\n  hub: nope\n", wantErr: `federation.hub "nope" must name a local knowledge world`},
		{name: "remote hub", body: validConfig + "federation:\n  hub: team-a\n", wantErr: `federation.hub "team-a" must name a local knowledge world`},
		{name: "negative interval", body: local + "federation:\n  hub: team-a\n  interval: -1s\n", wantErr: "must not be negative"},
		{
			name: "zero takes the default",
			body: local + "federation:\n  hub: team-a\n  quietPeriod: 0s\n",
			want: FederationConfig{Hub: "team-a", LeaseName: "demarkus-federation", QuietPeriod: 30 * time.Second, Interval: time.Minute},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(writeConfig(t, tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Federation != tt.want {
				t.Errorf("federation = %+v, want %+v", cfg.Federation, tt.want)
			}
		})
	}
}
