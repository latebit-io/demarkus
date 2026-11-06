// Package progress is the lifecycle's stderr log, one "[demarkus-memory]" line
// per step, the shape the session-start hooks have always relayed.
package progress

import (
	"fmt"
	"io"
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

// CloseOrWarn closes a resource nothing was written through: a failure loses
// no data, so it is logged, not returned.
func CloseOrWarn(c io.Closer, what string) {
	if err := c.Close(); err != nil {
		Warnf("close %s: %v", what, err)
	}
}
