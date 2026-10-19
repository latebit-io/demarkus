package knowledgeconfig

import (
	"strings"
	"testing"
)

const fileWorldConfig = `version: 1
tls:
  certFile: /run/demarkus/tls/tls.crt
  keyFile: /run/demarkus/tls/tls.key
worlds:
  - name: team
    authorities:
      - team.example.com
    dir: /var/lib/demarkus/worlds/team
    auth:
      tokensFile: /run/demarkus/team/tokens.toml
`

func TestParseFileWorld(t *testing.T) {
	config, err := Parse([]byte(fileWorldConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	world := config.Worlds[0]
	if !world.IsFile() || world.Dir != "/var/lib/demarkus/worlds/team" {
		t.Errorf("file world: IsFile %t, dir %q; want the configured directory", world.IsFile(), world.Dir)
	}
	if world.ID() != "" {
		t.Errorf("file world ID = %q, want none: nothing replicates a directory", world.ID())
	}
}

func TestValidateFileWorld(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"dir and bucket", strings.Replace(fileWorldConfig, "    dir:", "    bucket:\n      url: gs://team\n      worldID: 52b471f7-8d38-4c89-b44a-6f4f8b1a4f48\n    dir:", 1), "worlds[0].dir and worlds[0].bucket are exclusive"},
		{"neither", strings.Replace(fileWorldConfig, "    dir: /var/lib/demarkus/worlds/team\n", "", 1), "worlds[0].bucket.url"},
		{"relative dir", strings.Replace(fileWorldConfig, "dir: /var/lib", "dir: var/lib", 1), "worlds[0].dir must be a canonical absolute path"},
		{"uncleaned dir", strings.Replace(fileWorldConfig, "worlds/team\n", "worlds/team/\n", 1), "worlds[0].dir must be a canonical absolute path"},
		{"shared dir", fileWorldConfig + "  - name: other\n    authorities: [other.example.com]\n    dir: /var/lib/demarkus/worlds/team\n", "worlds[1].dir \"/var/lib/demarkus/worlds/team\" duplicates worlds[0].dir"},
		{"nested dir", fileWorldConfig + "  - name: inner\n    authorities: [inner.example.com]\n    dir: /var/lib/demarkus/worlds/team/inner\n", "worlds[1].dir \"/var/lib/demarkus/worlds/team/inner\" overlaps worlds[0].dir \"/var/lib/demarkus/worlds/team\": worlds must not nest"},
		{"enclosing dir", fileWorldConfig + "  - name: outer\n    authorities: [outer.example.com]\n    dir: /var/lib/demarkus\n", "worlds[1].dir \"/var/lib/demarkus\" overlaps worlds[0].dir \"/var/lib/demarkus/worlds/team\": worlds must not nest"},
		{"sibling with a shared prefix is fine", "", ""},
		{"peers", fileWorldConfig + "peers:\n  listen: \":6310\"\n  addresses: [\"10.0.0.2:6310\"]\n", "worlds[0].dir cannot be served with peers"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.body == "" {
				// A sibling whose name extends the other's is not nested.
				if _, err := Parse([]byte(fileWorldConfig + "  - name: sibling\n    authorities: [sibling.example.com]\n    dir: /var/lib/demarkus/worlds/team-archive\n")); err != nil {
					t.Fatalf("sibling directory refused: %v", err)
				}
				return
			}
			_, err := Parse([]byte(test.body))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error: got %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

// A file world takes the limits section like any world; the file store
// enforces maxDocuments on creates.
func TestFileWorldAcceptsLimits(t *testing.T) {
	config, err := Parse([]byte(fileWorldConfig + "    limits:\n      maxDocuments: 10\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.Worlds[0].Limits.MaxDocuments != 10 {
		t.Errorf("maxDocuments = %d, want 10", config.Worlds[0].Limits.MaxDocuments)
	}
}
