// Package bucketworlds opens knowledge worlds on object-storage buckets. A
// binary that imports it carries the GCS client; the local profile does not.
package bucketworlds

import (
	"context"
	"fmt"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
	"github.com/latebit-io/demarkus/server/internal/worldstore"
)

// MaxObjectBytes bounds one object a world's store writes or reads.
const MaxObjectBytes = bucketstore.MaxObjectBytes

// GCS is the production wiring: each world's bucket on Google Cloud Storage
// through one client built on the first bucket world and held for the
// process's life.
func GCS() worldstore.Factory {
	lazy := &gcs.Lazy{}
	return ForWorlds(func(ctx context.Context, world *knowledgeconfig.WorldConfig) (blob.Store, error) {
		return lazy.Open(ctx, world.Bucket.Name(), MaxObjectBytes)
	})
}

// ForWorlds opens each bucket world as a bucket store over the blob store
// objects opens for it; a test's in-memory buckets come in here.
func ForWorlds(objects func(context.Context, *knowledgeconfig.WorldConfig) (blob.Store, error)) worldstore.Factory {
	return func(ctx context.Context, world *knowledgeconfig.WorldConfig, hooks worldstore.Hooks) (worldstore.Store, error) {
		bucket, err := objects(ctx, world)
		if err != nil {
			return nil, fmt.Errorf("blob store: %w", err)
		}
		// Epoch = world ID, sequence = log sequence: a cursor resumes on any
		// replica and across a restart.
		store, err := bucketstore.Open(ctx, bucket, bucketstore.Options{
			WorldID:         world.Bucket.WorldID,
			Logger:          hooks.Logger.With("bucket", world.Bucket.Name()),
			RequestTimeout:  time.Duration(world.Limits.RequestTimeout),
			MaxDocuments:    world.Limits.MaxDocuments,
			ReadOnly:        world.ReadOnly,
			CheckpointGrace: time.Duration(world.Bucket.CheckpointGrace),
			Compaction:      role(hooks.Compaction),
			ChangeRing:      changefeed.DefaultRingSize,
			Committed:       hooks.Committed,
		})
		if err != nil {
			return nil, fmt.Errorf("bucket: %w", err)
		}
		return store, nil
	}
}

// role is the store's compaction role for the configured one.
func role(configured knowledgeconfig.CompactionRole) bucketstore.CompactionRole {
	if configured == knowledgeconfig.CompactionBackstop {
		return bucketstore.Backstop
	}
	return bucketstore.Eager
}
