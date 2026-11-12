package gate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// claimsDir holds one empty file per call a hook reported on, under ~/.demarkus.
const claimsDir = "gate-claims"

// claimTTL outlives the hooks of one call (seconds); older claims are swept.
const claimTTL = 10 * time.Minute

// A host call id is a file name here, so only a plain token is one.
var claimID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Claim reports whether this process is the one to report on call id. Each
// demarkus plugin hooks the same call and the host shows every hook's output,
// so the first to claim reports. No usable id or a failed claim also reports.
func Claim(id string) bool {
	if !claimID.MatchString(id) {
		return true
	}
	dir, err := config.StatePath(claimsDir)
	if err == nil {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "[demarkus-plugin] gate: claim: "+err.Error())
		return true
	}
	sweepClaims(dir, time.Now().Add(-claimTTL))
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
