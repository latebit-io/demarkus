package gate

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func claims(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".demarkus", claimsDir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestClaimReportsOncePerCall(t *testing.T) {
	home := setupHome(t, nil)
	if !Claim("toolu_01A") || Claim("toolu_01A") {
		t.Fatal("one call: want the first claim alone to report")
	}
	if !Claim("toolu_01B") {
		t.Error("another call did not report")
	}
	// No id to tell calls apart, or one that is no file name: always report.
	for _, id := range []string{"", "../escape", "a/b", "with space"} {
		for hook := range 2 {
			if !Claim(id) {
				t.Errorf("id %q, hook %d: did not report", id, hook)
			}
		}
	}
	if got := claims(t, home); len(got) != 2 {
		t.Errorf("claims = %v, want the two call ids alone", got)
	}
}

// The two plugins' hooks run side by side: exactly one reports.
func TestClaimIsExclusiveAcrossHooks(t *testing.T) {
	setupHome(t, nil)
	var reported atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if Claim("toolu_race") {
				reported.Add(1)
			}
		})
	}
	wg.Wait()
	if n := reported.Load(); n != 1 {
		t.Errorf("%d hooks reported, want 1", n)
	}
}

// Claims are evicted by age, so the directory holds recent calls only.
func TestClaimSweepsOldClaims(t *testing.T) {
	home := setupHome(t, nil)
	if !Claim("toolu_old") {
		t.Fatal("first claim did not report")
	}
	old := time.Now().Add(-2 * claimTTL)
	if err := os.Chtimes(filepath.Join(home, ".demarkus", claimsDir, "toolu_old"), old, old); err != nil {
		t.Fatal(err)
	}
	if !Claim("toolu_new") {
		t.Fatal("new claim did not report")
	}
	if got := claims(t, home); len(got) != 1 || got[0] != "toolu_new" {
		t.Errorf("claims = %v, want toolu_new alone", got)
	}
}

// An unwritable state directory reports rather than losing the warning.
func TestClaimReportsWhenItCannotRecord(t *testing.T) {
	home := setupHome(t, nil)
	if err := os.WriteFile(filepath.Join(home, ".demarkus", claimsDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for hook := range 2 {
		if !Claim("toolu_01A") {
			t.Errorf("hook %d did not report though the claim cannot be recorded", hook)
		}
	}
}
