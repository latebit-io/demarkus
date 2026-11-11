package lockdir

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// startProbeTimeout bounds one ps run: a stalled ps must not hold a lock wait.
const startProbeTimeout = 1500 * time.Millisecond

// lstartLayouts covers both C-locale ps orderings: BSD/macOS puts the day of the
// month before the month name, GNU after it.
var lstartLayouts = []string{"Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 2006"}

// ProcessStart reports when pid started. ok is false whenever that cannot be
// established, which every caller treats as "stay silent". /proc/<pid> ModTime
// is no start time (it can be when the entry was instantiated); ps lstart is.
func ProcessStart(pid int) (time.Time, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), startProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=") //nolint:gosec // fixed binary; the argument is an integer
	cmd.Env = append(os.Environ(), "LC_ALL=C")                                      // lstart is locale-formatted
	cmd.WaitDelay = 500 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, false
	}
	s := strings.Join(strings.Fields(string(out)), " ")
	for _, layout := range lstartLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
