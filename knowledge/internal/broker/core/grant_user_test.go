package core

import "testing"

// The grant names the verified person, canonical: the world stores it as the
// write's user whatever the request carried.
func TestWriteGrantNamesTheUser(t *testing.T) {
	world := WorldConfig{Name: "team", WriteScope: WriteScope{Paths: []string{"/**"}}}
	grant, err := WriteGrant(&world, &Claims{Email: "Alice@Example.com", EmailVerified: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := CanonicalEmail("Alice@Example.com"); grant.User != want || grant.Label != want {
		t.Errorf("grant user %q, label %q; want both %q", grant.User, grant.Label, want)
	}
}
