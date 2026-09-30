package filestore

import (
	"testing"

	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// OpenWatched is openWatched for the external test package in this directory.
func OpenWatched(t *testing.T, root string, ring int) (*Store, *changefeed.Hub) {
	return openWatched(t, root, ring)
}
