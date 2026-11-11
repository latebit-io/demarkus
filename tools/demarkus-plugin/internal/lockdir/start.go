package lockdir

import (
	"strconv"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision/probe"
)

// lstartLayouts covers both C-locale ps orderings: BSD/macOS puts the day of the
// month before the month name, GNU after it.
var lstartLayouts = []string{"Mon _2 Jan 15:04:05 2006", "Mon Jan _2 15:04:05 2006"}

// ProcessStart reports when pid started. ok is false whenever that cannot be
// established, which every caller treats as "stay silent". /proc/<pid> ModTime
// is no start time (it can be when the entry was instantiated); ps lstart is.
func ProcessStart(pid int) (time.Time, bool) {
	// lstart is locale-formatted.
	out, _ := probe.Bounded([]string{"LC_ALL=C"}, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	s := strings.Join(strings.Fields(out), " ")
	for _, layout := range lstartLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
