package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// claimsDir holds one empty file per call a hook reported on, under ~/.demarkus.
const claimsDir = "gate-claims"

// claimTTL outlives the hooks of one call (seconds); older claims are swept.
const claimTTL = 10 * time.Minute

// shownDir holds one empty file per taint warning a session showed its user.
const shownDir = "gate-shown"

// shownTTL evicts a session's record; one that outlives it is warned again.
const shownTTL = 24 * time.Hour

// A host call id is a file name here, so only a plain token is one.
var claimID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Claim reports whether this process is the one to report on call id. Each
// demarkus plugin hooks the same call and the host shows every hook's output,
// so the first to claim reports. No usable id or a failed claim also reports.
func Claim(id string) bool { return claim(claimsDir, claimTTL, id) }

// Report is a post hook's part in d: ok when this process is the one to show
// the warn, notice what its user sees of it. The call is claimed first: the
// plugin that loses it must not spend the session's one showing.
func Report(d Decision, in *Input) (notice string, ok bool) {
	if d.Decision != "warn" || !Claim(in.ToolUseID) {
		return "", false
	}
	return shown(d, in.SessionID), true
}

// shown is the part of a warn a session's user sees. The taint warning
// repeats on every write after one untrusted read, so it shows once per set
// of sources, never at quiet; no session id or a failed claim shows it every time.
func shown(d Decision, session string) string {
	if len(d.sources) == 0 {
		return d.Reason
	}
	if d.quiet {
		return d.rest
	}
	if session == "" {
		return d.Reason
	}
	sum := sha256.Sum256([]byte(strings.Join(d.sources, "\n")))
	if claim(shownDir, shownTTL, session+"-"+hex.EncodeToString(sum[:8])) {
		return d.Reason
	}
	return d.rest
}

// claim creates the file name under dir, sweeping files older than ttl; true
// when this call created it, or when it cannot tell.
func claim(dir string, ttl time.Duration, name string) bool {
	if !claimID.MatchString(name) {
		return true
	}
	dir, err := config.StatePath(dir)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: claim: "+err.Error())
		return true
	}
	sweepClaims(dir, time.Now().Add(-ttl))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: claim: "+err.Error())
		return true
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: claim: "+err.Error())
	}
	return true
}

// sweepClaims removes claims last touched before cutoff. A failure is logged:
// a leftover claim is an empty file the next sweep retries.
func sweepClaims(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: sweep claims: "+err.Error())
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // another hook swept it
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: sweep claims: "+err.Error())
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: sweep claims: "+err.Error())
		}
	}
}
