package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/registrytest"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

func TestProjectBindSetUsesTransactionWriter(t *testing.T) {
	home := registrytest.SetupHome(t)
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "remote", Host: "mark://remote.example", TokenFile: "-"})
	registrytest.FailWritesTo(t, "project-souls")
	if err := BindProject(filepath.Join(home, "repo"), "remote"); err == nil {
		t.Fatal("BindProject succeeded despite injected write failure")
	}
	path, err := config.StatePath("project-souls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding file exists after failed write: %v", err)
	}
}

func TestProjectBindSetRejectsRecordDelimiters(t *testing.T) {
	registrytest.SetupHome(t)
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "remote", Host: "mark://remote.example", TokenFile: "-"})
	for _, dir := range []string{"/repo\tother", "/repo\rnext", "/repo\nnext", "/repo\x00next", " relative", "relative "} {
		if err := BindProject(dir, "remote"); err == nil {
			t.Errorf("binding directory %q should error", dir)
		}
	}
	path, err := config.StatePath("project-souls")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binding file exists after rejected inputs: %v", err)
	}
}

func TestDeriveSlug(t *testing.T) {
	cases := map[string]string{
		"mcp.broker.acme.com": "acme",
		"MCP.BROKER.Acme.com": "acme",
		"broker.acme.com":     "acme",
		"acme-broker.example": "acme-broker",
		"soul.demarkus.io":    "soul",
	}
	for host, want := range cases {
		if got := DeriveSlug(host); got != want {
			t.Errorf("DeriveSlug(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestSetLocalMemoryAlias(t *testing.T) {
	home := registrytest.SetupHome(t)
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "knowledge-systems"), []byte("corp\nacme-memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, alias, want string
		wantErr           bool
	}{
		{name: "collides with knowledge system", alias: "corp", wantErr: true},
		{name: "captures a plugin-prefixed store", alias: "memory", wantErr: true},
		{name: "bad characters", alias: "Bad Name", wantErr: true},
		{name: "records", alias: "brain", want: "brain"},
		{name: "unchanged", alias: "brain", want: "brain"},
		{name: "default id clears", alias: config.LocalMemoryID, want: ""},
		{name: "records again", alias: "vault", want: "vault"},
		{name: "empty clears", alias: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, token, err := SetLocalAlias(tt.alias)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			alias, stored, err := config.LocalMemoryAliasRecord()
			if err != nil {
				t.Fatal(err)
			}
			if alias != tt.want || token == "" || stored != token {
				t.Fatalf("record = %q %q, returned token %q; want alias %q", alias, stored, token, tt.want)
			}
		})
	}
}

func TestRestoreLocalMemoryAliasOnlyForOwnWrite(t *testing.T) {
	home := registrytest.SetupHome(t)
	storedAliasBefore := ""
	set := func(alias string) string {
		t.Helper()
		previous, token, err := SetLocalAlias(alias)
		if err != nil {
			t.Fatal(err)
		}
		// the replaced alias is observed under the lock, so restore never uses a stale read
		if want := storedAliasBefore; previous != want {
			t.Fatalf("set(%q): previous = %q, want %q", alias, previous, want)
		}
		storedAliasBefore = alias
		return token
	}
	restore := func(previous, token string) {
		t.Helper()
		if err := RestoreLocalAlias(previous, token); err != nil {
			t.Fatal(err)
		}
		storedAliasBefore = storedAlias(t)
	}
	tokA := set("alpha")
	tokB := set("beta")
	// command A fails after B recorded: no rollback
	restore("", tokA)
	if got := storedAlias(t); got != "beta" {
		t.Fatalf("alias = %q, want beta kept", got)
	}
	// B fails: its own write is rolled back to what it saw
	restore("alpha", tokB)
	if got := storedAlias(t); got != "alpha" {
		t.Fatalf("alias = %q, want alpha restored", got)
	}
	// ABA: C re-records the value A wrote; A's rollback must not touch C's write
	tokA = set("beta")
	set("gamma")
	set("beta")
	restore("alpha", tokA)
	if got := storedAlias(t); got != "beta" {
		t.Fatalf("alias = %q, want C's beta kept", got)
	}
	// the previous value was joined as a store meanwhile: the owned record is
	// cleared rather than left pointing at a server that never started
	tokD := set("delta")
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "knowledge-systems"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := RestoreLocalAlias("beta", tokD)
	if err == nil || !strings.Contains(err.Error(), "alias cleared instead") {
		t.Fatalf("restore onto a joined name: err = %v", err)
	}
	if got := storedAlias(t); got != "" {
		t.Fatalf("alias = %q, want cleared", got)
	}
	// same collision, but a later server owns the record: untouched
	storedAliasBefore = ""
	set("epsilon")
	if err := RestoreLocalAlias("beta", tokD); err != nil {
		t.Fatal(err)
	}
	if got := storedAlias(t); got != "epsilon" {
		t.Fatalf("alias = %q, want epsilon kept", got)
	}
}

func TestMemoryEndpointResolvesLocalAndRemoteRows(t *testing.T) {
	home := registrytest.SetupHome(t)
	if _, err := Resolve(config.LocalMemoryID); err == nil {
		t.Fatal("no local config must be an error")
	}
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "plugin-memory.conf"), []byte("SOUL_DIR=/no/such\nPORT=6310\nMODE=default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ep, err := Resolve(config.LocalMemoryID)
	if err != nil || ep.Host != "mark://localhost:6310" || !ep.Insecure || ep.Token != "" || ep.Broker {
		t.Fatalf("local endpoint without a token file = %+v, err=%v", ep, err)
	}
	if err := os.WriteFile(filepath.Join(home, ".demarkus", "plugin-memory.token"), []byte(" tok \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ep, err = Resolve(config.LocalMemoryID); err != nil || ep.Token != "tok" {
		t.Fatalf("local token not read: %+v, err=%v", ep, err)
	}
	if _, err := Resolve("nope"); err == nil {
		t.Fatal("an unknown catalog id must be an error")
	}
	tokenFile := filepath.Join(home, ".demarkus", "soul-soul.token")
	if err := os.WriteFile(tokenFile, []byte("remote-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "soul", Host: "mark://soul.demarkus.io", Insecure: true, TokenFile: tokenFile})
	if ep, err = Resolve("soul"); err != nil || ep.Token != "remote-token" || !ep.Insecure || ep.Broker {
		t.Fatalf("remote endpoint = %+v, err=%v", ep, err)
	}
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve("soul"); err == nil {
		t.Fatal("a remote row whose token file is missing must be an error")
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "bare", Host: "mark://bare.example", TokenFile: "-"})
	if ep, err = Resolve("bare"); err != nil || ep.Token != "" {
		t.Fatalf("a tokenless row resolves without a token: %+v, err=%v", ep, err)
	}
	registrytest.RegisterRow(t, config.MemoryRow{Slug: "team", Host: "https://broker.example", TokenFile: "-"})
	if ep, err = Resolve("team"); err != nil || !ep.Broker || ep.Token != "" {
		t.Fatalf("a broker row is flagged and carries no token: %+v, err=%v", ep, err)
	}
}

func storedAlias(t *testing.T) string {
	t.Helper()
	alias, err := config.LocalMemoryAlias()
	if err != nil {
		t.Fatal(err)
	}
	return alias
}
