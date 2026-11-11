package filestore

import (
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// OpenWatched is openWatched for the external test package in this directory.
func OpenWatched(t *testing.T, root string, ring int) (*Store, *changefeed.Hub) {
	return openWatched(t, root, ring)
}

// PollOutsideEvery is opts with the outside-write poll at interval.
func PollOutsideEvery(opts Options, interval time.Duration) Options {
	opts.outsidePoll = interval
	return opts
}
