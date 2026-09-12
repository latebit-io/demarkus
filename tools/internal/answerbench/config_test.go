package answerbench

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReaderConfigAndEnvironmentIsolation(t *testing.T) {
	raw, err := readerConfig(&Config{Port: 16319, Steps: 8, MCP: "/mcp", Proxy: "/proxy"}, "/fresh-session")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Permission map[string]string `json:"permission"`
		MCP        map[string]struct {
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if config.Permission["*"] != "deny" || config.Permission["fixture_*"] != "allow" || len(config.MCP) != 1 || config.MCP["fixture"].Environment["HOME"] != "/fresh-session" {
		t.Fatalf("reader escaped fixture isolation: %s", raw)
	}
	env := cleanEnv([]string{"PATH=/bin", "OPENCODE_CONFIG_CONTENT=private", "OPENCODE_EXPERIMENTAL=1", "DEMARKUS_AUTH=private", "XDG_CONFIG_HOME=/private"})
	if strings.Join(env, ";") != "PATH=/bin" {
		t.Fatalf("inherited benchmark-sensitive environment: %v", env)
	}
}

func TestCitationLocationScope(t *testing.T) {
	for _, raw := range []string{"mark://elsewhere/x.md/v1#section", "https://127.0.0.1:16319/x.md", "mark://user@127.0.0.1:16319/x.md", "/x.md?secret=1", "relative.md"} {
		if _, err := location(raw, "127.0.0.1:16319"); err == nil {
			t.Errorf("out-of-scope location accepted: %s", raw)
		}
	}
	if got, err := location("mark://soul.example:6309/doc.md/v3#part", "soul.example"); err != nil || got.Version != 3 || got.Path != "/doc.md" {
		t.Fatalf("equivalent origin rejected: %+v, %v", got, err)
	}
}
