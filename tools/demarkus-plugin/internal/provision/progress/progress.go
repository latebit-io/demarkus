// Package progress is the lifecycle's stderr log, one "[demarkus-memory]" line
// per step, the shape the session-start hooks have always relayed.
package progress

import (
	"fmt"
	"os"
)

// Logf writes one progress line.
func Logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] "+format+"\n", a...)
}

// Warnf writes one warning line.
func Warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[demarkus-memory] warning: "+format+"\n", a...)
}
