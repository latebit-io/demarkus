package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	gcsclient "cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/blob/gcs"
)

// maxStateObjectBytes bounds one record in the state bucket.
const maxStateObjectBytes = 64 << 10

// stateProbeTimeout bounds the startup reachability check.
const stateProbeTimeout = 30 * time.Second

// OpenStateBucket binds server.stateBucket over client and lists an empty
// prefix, so a missing bucket or grant fails startup, not the first login.
func OpenStateBucket(client *gcsclient.Client, cfg *core.ServerConfig, log *slog.Logger) (blob.Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateProbeTimeout)
	defer cancel()
	objects, err := gcs.New(client, cfg.StateBucketName(), maxStateObjectBytes)
	if err == nil {
		_, err = objects.List(ctx, "probe/", "", "")
	}
	if err != nil {
		return nil, fmt.Errorf("state bucket %s: %w", cfg.StateBucket, err)
	}
	log.Info("broker: state bucket ready", "bucket", cfg.StateBucket)
	return objects, nil
}
