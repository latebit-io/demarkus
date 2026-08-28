package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/sync/errgroup"
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

// NewGCSBuckets opens a GCS client and wires the bucket creator from
// provisioning config; the returned cleanup closes the client. The one
// wiring site for serving setup and the deprovision flow.
func NewGCSBuckets(cfg *Config, log *slog.Logger) (BucketCreator, func(), error) {
	gcsClient, err := storage.NewClient(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("GCS client unavailable: %w", err)
	}
	cleanup := func() {
		if closeErr := gcsClient.Close(); closeErr != nil {
			log.Warn("GCS client close failed", "err", closeErr)
		}
	}
	buckets, err := newGCSBucketCreator(gcsClient, cfg.Provisioning.BucketProject, cfg.Provisioning.BucketLocation, log)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return buckets, cleanup, nil
}

// newGCSBucketCreator wires the production bucket creator. project is
// required; location may be empty (GCS default).
func newGCSBucketCreator(client *storage.Client, project, location string, log *slog.Logger) (*GCSBucketCreator, error) {
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
	// Deliberately no object versioning: it would keep retention-pruned
	// blobs recoverable (breaking permanent deletion) and block
	// -delete-bucket; GCS soft delete covers accidental deletion.
	return &storage.BucketAttrs{
		Location:                 location,
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
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

// DeleteBucket permanently removes a tenant bucket and every object
// generation (deprovision only; absent = success). Soft-delete disable
// enforces within ~30s, so purge-and-delete retries until convergent.
func (c *GCSBucketCreator) DeleteBucket(ctx context.Context, bucket string) error {
	handle := c.client.Bucket(bucket)
	// Disable soft delete first so object deletions purge instead of
	// lingering and blocking the bucket delete; also covers adopted
	// buckets that were created with versioning on.
	if _, err := handle.Update(ctx, storage.BucketAttrsToUpdate{
		SoftDeletePolicy: &storage.SoftDeletePolicy{RetentionDuration: 0},
	}); err != nil {
		if errors.Is(err, storage.ErrBucketNotExist) {
			return nil
		}
		return fmt.Errorf("disable soft delete on bucket %q: %w", bucket, err)
	}
	backoff := 5 * time.Second
	for {
		err := c.purgeAndDelete(ctx, bucket)
		if err == nil {
			return nil
		}
		// Objects soft-deleted before the policy change enforced keep
		// the bucket non-empty until they purge; wait and re-run.
		c.log.Warn("bucket delete not yet convergent; retrying", "bucket", bucket, "backoff", backoff, "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("delete bucket %q: %w (last: %v)", bucket, ctx.Err(), err)
		case <-time.After(backoff):
		}
		if backoff < 40*time.Second {
			backoff *= 2
		}
	}
}

// purgeAndDelete removes every listable generation then the bucket.
func (c *GCSBucketCreator) purgeAndDelete(ctx context.Context, bucket string) error {
	handle := c.client.Bucket(bucket)
	query := &storage.Query{Versions: true}
	// Names + generations only: the deletes need nothing else.
	if err := query.SetAttrSelection([]string{"Name", "Generation"}); err != nil {
		return fmt.Errorf("bucket %q attr selection: %w", bucket, err)
	}
	it := handle.Objects(ctx, query)
	// Bounded-parallel deletes: a soul holds thousands of generations
	// and strictly sequential round trips take minutes.
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(16)
	for {
		object, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("list bucket %q: %w", bucket, err)
		}
		name, generation := object.Name, object.Generation
		group.Go(func() error {
			// Versions:true listings always carry a generation.
			obj := handle.Object(name).Generation(generation)
			if err := obj.Delete(groupCtx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
				return fmt.Errorf("delete object %s/%s (gen %d): %w", bucket, name, generation, err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	if err := handle.Delete(ctx); err != nil && !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("delete bucket %q: %w", bucket, err)
	}
	return nil
}
