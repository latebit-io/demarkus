package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// GCSBucketCreator provisions per-tenant GCS buckets idempotently
// (bucket-per-tenant decision, memory-broker plan 2026-08-28 addendum;
// GCS's ~1 bucket op/2s/project cap is fine at alpha signup rates).
type GCSBucketCreator struct {
	client   *storage.Client
	project  string
	location string
	log      *slog.Logger
}

// NewGCSBucketCreator wires the production bucket creator. project is
// required; location may be empty (GCS default).
func NewGCSBucketCreator(client *storage.Client, project, location string, log *slog.Logger) (*GCSBucketCreator, error) {
	if client == nil {
		return nil, errors.New("broker: gcs bucket creator requires a storage client")
	}
	if project == "" {
		return nil, errors.New("broker: gcs bucket creator requires provisioning.bucketProject")
	}
	if log == nil {
		log = slog.Default()
	}
	return &GCSBucketCreator{client: client, project: project, location: location, log: log}, nil
}

// EnsureBucket creates the bucket when absent (uniform bucket-level
// access, enforced public-access prevention); a pre-existing bucket is
// adopted only after its lockdown is verified.
func (c *GCSBucketCreator) EnsureBucket(ctx context.Context, bucket string) error {
	handle := c.client.Bucket(bucket)
	existing, err := handle.Attrs(ctx)
	if err == nil {
		return verifyBucketLockdown(bucket, existing)
	}
	if !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("check bucket %q: %w", bucket, err)
	}
	if createErr := handle.Create(ctx, c.project, tenantBucketAttrs(c.location)); createErr != nil {
		// A racing replica may have won; adopt only a bucket that now
		// exists AND passes lockdown. Once the bucket exists the create
		// error is stale, so it rides the log, not the return.
		raced, attrsErr := handle.Attrs(ctx)
		if attrsErr != nil {
			return fmt.Errorf("create bucket %q: %w", bucket, createErr)
		}
		if lockErr := verifyBucketLockdown(bucket, raced); lockErr != nil {
			c.log.Warn("bucket create failed and the existing bucket is not locked down",
				"bucket", bucket, "createErr", createErr)
			return lockErr
		}
		c.log.Warn("bucket create raced an existing bucket; adopted after lockdown check",
			"bucket", bucket, "createErr", createErr)
	}
	return nil
}

// tenantBucketAttrs is the ONE definition of the tenant-bucket posture;
// verifyBucketLockdown must assert every security-relevant field set
// here, or adopted pre-existing buckets dodge the new requirement.
func tenantBucketAttrs(location string) *storage.BucketAttrs {
	return &storage.BucketAttrs{
		Location:                 location,
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		// Backup posture, not security: object versioning shields the
		// mutable head.json from accidental overwrite or deletion, so
		// the verifier does not gate on it.
		VersioningEnabled: true,
	}
}

// verifyBucketLockdown fails closed on a bucket that does not carry the
// tenantBucketAttrs posture (location aside, which is create-time only).
func verifyBucketLockdown(bucket string, attrs *storage.BucketAttrs) error {
	if !attrs.UniformBucketLevelAccess.Enabled {
		return fmt.Errorf("bucket %q does not enforce uniform bucket-level access; refusing to adopt it for tenant data", bucket)
	}
	if attrs.PublicAccessPrevention != storage.PublicAccessPreventionEnforced {
		return fmt.Errorf("bucket %q does not enforce public-access prevention; refusing to adopt it for tenant data", bucket)
	}
	return nil
}

// DeleteBucket permanently removes a tenant bucket and every object in
// it. Only the explicit deprovision flow calls this; absent is success
// so reruns converge.
func (c *GCSBucketCreator) DeleteBucket(ctx context.Context, bucket string) error {
	handle := c.client.Bucket(bucket)
	it := handle.Objects(ctx, nil)
	for {
		object, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list bucket %q: %w", bucket, err)
		}
		if err := handle.Object(object.Name).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return fmt.Errorf("delete object %s/%s: %w", bucket, object.Name, err)
		}
	}
	if err := handle.Delete(ctx); err != nil && !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("delete bucket %q: %w", bucket, err)
	}
	return nil
}
