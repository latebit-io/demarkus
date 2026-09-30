package core

import (
	"slices"
	"strings"
	"testing"
)

func TestLoadConfigAgentTokens(t *testing.T) {
	clearConfigEnv(t)
	const teamB = "  - name: team-b\n    namespace: team-a\n    tokensSecret: team-b-tokens\n    defaultToken:\n      paths: [\"/b\"]\n"
	tests := []struct {
		name     string
		body     string
		wantErr  string
		validate func(*testing.T, *Config)
	}{
		{
			name: "defaults paths",
			body: validConfig + "agentTokens:\n  - world: team-a\n    secret: team-a-token-values\n    key: admin\n",
			validate: func(t *testing.T, c *Config) {
				if got := c.AgentTokens[0].Paths; !slices.Equal(got, []string{"/**"}) {
					t.Errorf("paths = %v, want [/**]", got)
				}
			},
		},
		{
			name: "keeps explicit paths",
			body: validConfig + "agentTokens:\n  - world: team-a\n    secret: v\n    key: admin\n    paths: [\"/hub/**\"]\n",
			validate: func(t *testing.T, c *Config) {
				if got := c.AgentTokens[0].Paths; !slices.Equal(got, []string{"/hub/**"}) {
					t.Errorf("paths = %v", got)
				}
			},
		},
		{
			name:    "unknown world",
			body:    validConfig + "agentTokens:\n  - world: nope\n    secret: v\n    key: admin\n",
			wantErr: `world "nope" is not a configured worlds[] entry`,
		},
		{
			name: "memory world",
			body: strings.Replace(validConfig, `publicURL: "https://broker.example.com"`,
				"publicURL: \"https://broker.example.com\"\n  memory:\n    publicURL: \"https://memory.example.com\"", 1) +
				"    profile: knowledge\n  - name: mem\n    profile: memory\n    namespace: mem\n    tokensSecret: mem-tokens\n    allow:\n      emails: [\"a@example.com\"]\n    writeScope:\n      paths: [\"/**\"]\nagentTokens:\n  - world: mem\n    secret: v\n    key: admin\n",
			wantErr: `world "mem" is a memory world`,
		},
		{
			name:    "duplicate world",
			body:    validConfig + "agentTokens:\n  - world: team-a\n    secret: v\n    key: admin\n  - world: team-a\n    secret: w\n    key: admin\n",
			wantErr: `duplicate world "team-a"`,
		},
		{
			name:    "bad secret name",
			body:    validConfig + "agentTokens:\n  - world: team-a\n    secret: Bad_Name\n    key: admin\n",
			wantErr: "must be a lowercase DNS subdomain",
		},
		{
			name:    "missing key",
			body:    validConfig + "agentTokens:\n  - world: team-a\n    secret: v\n",
			wantErr: `key ""`,
		},
		{
			name:    "world tokens Secret",
			body:    validConfig + "agentTokens:\n  - world: team-a\n    secret: team-a-tokens\n    key: admin\n",
			wantErr: `is the tokens Secret of world "team-a"`,
		},
		{
			name:    "another world's tokens Secret in the same namespace",
			body:    validConfig + teamB + "agentTokens:\n  - world: team-a\n    secret: team-b-tokens\n    key: admin\n",
			wantErr: `is the tokens Secret of world "team-b"`,
		},
		{
			name:    "shared Secret key across worlds",
			body:    validConfig + teamB + "agentTokens:\n  - world: team-a\n    secret: v\n    key: admin\n  - world: team-b\n    secret: v\n    key: admin\n",
			wantErr: `is also the agent token of world "team-a"`,
		},
		{
			name: "file backend",
			body: fileBackendConfig(func(s string) string {
				return s + "agentTokens:\n  - world: team-a\n    secret: v\n    key: admin\n"
			}),
			wantErr: `agentTokens requires storage.backend "kubernetes"`,
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
			tt.validate(t, cfg)
		})
	}
}
