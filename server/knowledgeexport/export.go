// Package knowledgeexport streams one pinned GCS world snapshot.
package knowledgeexport

import (
	"context"
	"fmt"

	"cloud.google.com/go/storage"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
)

const maxObjectBytes = 4 << 20

// Exporter reads exact retained document bytes from one GCS world.
type Exporter struct {
	objects blob.Store
	worldID string
}

// New binds an exporter to one GCS bucket and immutable world identity.
func New(client *storage.Client, bucket, worldID string) (*Exporter, error) {
	objects, err := gcs.New(client, bucket, maxObjectBytes)
	if err != nil {
		return nil, fmt.Errorf("create knowledge exporter: %w", err)
	}
	return &Exporter{objects: objects, worldID: worldID}, nil
}

// ExportDocs streams every document from one root pinned at export start.
func (exporter *Exporter) ExportDocs(ctx context.Context, fn func(string, protocolstore.StoredDocument) error) error {
	if exporter == nil {
		return fmt.Errorf("export knowledge world: exporter is nil")
	}
	return bucketstore.ExportDocs(ctx, exporter.objects, exporter.worldID, 0, fn)
}
