package snirouter

import (
	"errors"
	"testing"
)

// A catch-all mapping answers the server names no other mapping holds,
// including none, as a single-world server must for a client dialing by IP.
func TestCatchAllRoutesEveryOtherServerName(t *testing.T) {
	soul, named := testEndpoint("soul"), testEndpoint("named")
	router, err := New([]Mapping{{Authority: AnyAuthority, Endpoint: soul}, {Authority: "named.example", Endpoint: named}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for serverName, want := range map[string]any{"": soul, "localhost": soul, "127.0.0.1": soul, "Named.Example": named} {
		got, err := router.lookup(serverName)
		if err != nil || got != want {
			t.Errorf("lookup(%q) = %v, %v; want %v", serverName, got, err, want)
		}
	}
	if _, err := New([]Mapping{{Authority: AnyAuthority, Endpoint: soul}, {Authority: AnyAuthority, Endpoint: named}}); !errors.Is(err, ErrDuplicateAuthority) {
		t.Errorf("two catch-alls: err %v, want ErrDuplicateAuthority", err)
	}
	strict, err := New([]Mapping{{Authority: "named.example", Endpoint: named}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := strict.lookup(""); err == nil {
		t.Error("a router without a catch-all routed an empty server name")
	}
}
