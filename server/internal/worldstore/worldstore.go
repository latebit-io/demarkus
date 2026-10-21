// Package worldstore is the contract between the knowledge server and the
// store that backs one of its worlds, so a binary chooses its backends.
package worldstore

import (
	"context"
	"io"
	"log/slog"

	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

// Store is a world's store as the server holds it: contract types only.
// Close ends its watches and refuses later writes (backend.ErrClosed); a
// store several replicas share also implements backend.Follower.
type Store interface {
	backend.Store
	// CurrentReader is what another world's commit reads a policy through.
	backend.CurrentReader
	backend.ChangeSource
	io.Closer
}

// Hooks is what the server hands a world's store at open.
type Hooks struct {
	Logger *slog.Logger
	// Committed runs with each sequence this replica learns its store
	// committed through, by its own commit or one it lost a race to.
	Committed func(sequence int64)
	// Compaction is this replica's part in checkpointing shared storage.
	Compaction knowledgeconfig.CompactionRole
}

// Factory opens a world's store, creating the world in empty storage unless
// it is read-only. A binary supplies one for the backends it carries.
type Factory func(ctx context.Context, world *knowledgeconfig.WorldConfig, hooks Hooks) (Store, error)
