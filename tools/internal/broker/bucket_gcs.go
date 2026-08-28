package broker

import (
	"context"
	"errors"
	"fmt"

	"cloud.google.com/go/storage"
)

// GCSBucketCreator provisions per-tenant GCS buckets idempotently
// (bucket-per-tenant decision, memory-broker plan 2026-08-28 addendum;
// GCS's ~1 bucket op/2s/project cap is fine at alpha signup rates).
type GCSBucketCreator struct {
	client   *storage.Client
	project  string
	location string
}

// NewGCSBucketCreator wires the production bucket creator. project is
// required; location may be empty (GCS default).
func NewGCSBucketCreator(client *storage.Client, project, location string) (*GCSBucketCreator, error) {
	if client == nil {
		return nil, errors.New("broker: gcs bucket creator requires a storage client")
	}
	if project == "" {
		return nil, errors.New("broker: gcs bucket creator requires provisioning.bucketProject")
	}
	return &GCSBucketCreator{client: client, project: project, location: location}, nil
}

// EnsureBucket creates the bucket when absent. Buckets are locked down:
// uniform bucket-level access and enforced public-access prevention,
// since only the knowledge server's service account ever reads them.
func (c *GCSBucketCreator) EnsureBucket(ctx context.Context, bucket string) error {
	handle := c.client.Bucket(bucket)
	_, err := handle.Attrs(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("check bucket %q: %w", bucket, err)
	}
	attrs := &storage.BucketAttrs{
		Location:                 c.location,
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
	}
	if err := handle.Create(ctx, c.project, attrs); err != nil {
		// A concurrent replica may have won the race; conflict on an
		// already-owned bucket converges to success on the next Attrs.
		if _, attrsErr := handle.Attrs(ctx); attrsErr == nil {
			return nil
		}
		return fmt.Errorf("create bucket %q: %w", bucket, err)
	}
	return nil
}
