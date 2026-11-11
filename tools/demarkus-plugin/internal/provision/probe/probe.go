// Package probe runs the read-only discovery commands (ps, pgrep, a
// supervisor's status) under one budget, so a stalled one cannot hold a
// session start or a lock wait.
package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Timeout is the whole budget for one probe; KillGrace is the slice of it
// reserved for pipe cleanup.
const (
	Timeout   = 2 * time.Second
	KillGrace = 500 * time.Millisecond
)

// Bounded runs a discovery command under the budget and returns its trimmed
// output. Empty on any failure, matching its callers' best-effort contract;
// nil env inherits ours.
func Bounded(env []string, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout-KillGrace)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.WaitDelay = KillGrace
	out, err := cmd.Output()
	if err != nil {
		// A clean non-zero exit is an answer (pgrep says "no match" that way);
		// only a failure to run at all leaves us without one.
		var exit *exec.ExitError
		return "", errors.As(err, &exit) && ctx.Err() == nil
	}
	return strings.TrimSpace(string(out)), true
}
