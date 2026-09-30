package backend

import "github.com/latebit-io/demarkus/server/internal/changefeed"

// ChangeSource is a store feeding a change hub under its own durable epoch
// and commit sequence, so a WATCH cursor resumes across a restart on every
// backend. Contract: mark://soul.demarkus.io/adr/0028-stores-own-a-durable-change-sequence.md
type ChangeSource interface {
	// Changes is the hub the store's commits feed; nil when WATCH is off.
	Changes() *changefeed.Hub
}
