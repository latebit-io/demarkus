package gate

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func claims(t *testing.T, home, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".demarkus", dir))
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
	if got := claims(t, home, claimsDir); len(got) != 2 {
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
	if got := claims(t, home, claimsDir); len(got) != 1 || got[0] != "toolu_new" {
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

// The taint warning is the same on every write: a session's user sees it once
// per set of sources, and the other warnings every time.
func TestShownOncePerSession(t *testing.T) {
	home := setupHome(t, nil)
	d := Decision{Decision: "warn", Reason: "tags Also: taint", rest: "tags", sources: []string{"shell download: curl"}}
	if got := shown(d, "session-a"); got != d.Reason {
		t.Fatalf("first write shows %q, want the whole reason", got)
	}
	if got := shown(d, "session-a"); got != "tags" {
		t.Errorf("second write shows %q, want the other warnings alone", got)
	}
	grown := d
	grown.sources = append(slices.Clone(d.sources), "WebSearch")
	if got := shown(grown, "session-a"); got != d.Reason {
		t.Errorf("a new source shows %q, want the whole reason", got)
	}
	if got := shown(d, "session-b"); got != d.Reason {
		t.Errorf("another session shows %q, want the whole reason", got)
	}
	// No session to scope it to, or an id that is no file name: every time.
	for _, session := range []string{"", "../escape"} {
		for write := range 2 {
			if got := shown(d, session); got != d.Reason {
				t.Errorf("session %q, write %d shows %q, want the whole reason", session, write, got)
			}
		}
	}
	if got := shown(Decision{Decision: "warn", Reason: "tags"}, "session-a"); got != "tags" {
		t.Errorf("a warn without taint shows %q, want it whole", got)
	}
	if got := claims(t, home, shownDir); len(got) != 3 {
		t.Errorf("shown = %v, want one per session and source set", got)
	}
}

// A shown record is evicted by age: a session that outlives it is warned again.
func TestShownSweepsOldRecords(t *testing.T) {
	home := setupHome(t, nil)
	d := Decision{Decision: "warn", Reason: "taint", sources: []string{"WebSearch"}}
	if shown(d, "session-a") != "taint" || shown(d, "session-a") != "" {
		t.Fatal("want the warning shown once")
	}
	dir := filepath.Join(home, ".demarkus", shownDir)
	old := time.Now().Add(-2 * shownTTL)
	for _, name := range claims(t, home, shownDir) {
		if err := os.Chtimes(filepath.Join(dir, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := shown(d, "session-a"); got != "taint" {
		t.Errorf("after the record aged out the write shows %q, want the warning", got)
	}
	if got := claims(t, home, shownDir); len(got) != 1 {
		t.Errorf("shown = %v, want the fresh record alone", got)
	}
}

// One hook reports a call's warn; the one that loses the call spends no
// showing, and nothing but a warn is reported.
func TestReport(t *testing.T) {
	home := setupHome(t, nil)
	d := Decision{Decision: "warn", Reason: "taint", sources: []string{"WebSearch"}}
	in := &Input{ToolUseID: "toolu_01A", SessionID: "session-a"}
	if notice, ok := Report(d, in); !ok || notice != "taint" {
		t.Fatalf("first hook = %q, %v; want the warning reported", notice, ok)
	}
	if _, ok := Report(d, in); ok {
		t.Error("the second hook on the call reported too")
	}
	if _, ok := Report(Decision{Decision: "allow"}, &Input{ToolUseID: "toolu_01B"}); ok {
		t.Error("an allow was reported")
	}
	if got := claims(t, home, claimsDir); len(got) != 1 {
		t.Errorf("claims = %v, want the warned call alone", got)
	}
	if got := claims(t, home, shownDir); len(got) != 1 {
		t.Errorf("shown = %v, want one record", got)
	}
}
