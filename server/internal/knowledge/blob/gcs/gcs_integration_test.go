//go:build gcsintegration

package gcs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blobtest"
)

const cleanupTimeout = 2 * time.Minute

func TestGCSIntegration(t *testing.T) {
	bucket := os.Getenv("DEMARKUS_TEST_GCS_BUCKET")
	if bucket == "" {
		if os.Getenv("DEMARKUS_TEST_GCS_REQUIRED") == "1" {
			t.Fatal("DEMARKUS_TEST_GCS_BUCKET is required")
		}
		t.Skip("DEMARKUS_TEST_GCS_BUCKET is not set")
	}

	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate test prefix: %v", err)
	}
	prefix := "_demarkus/test/" + hex.EncodeToString(random[:]) + "/"
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		t.Fatalf("create GCS client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close GCS client: %v", err)
		}
	})
	store, err := gcs.New(client, bucket, 2<<20)
	if err != nil {
		t.Fatalf("create GCS store: %v", err)
	}
	t.Cleanup(func() {
		cleanupObjects(t, store, prefix)
	})

	blobtest.RunConformance(t, store, prefix)
}

func cleanupObjects(t *testing.T, store blob.Store, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	for {
		result, err := store.List(ctx, prefix, "")
		if err != nil {
			if ctx.Err() != nil {
				t.Errorf("cleanup %q exceeded %s: %v", prefix, cleanupTimeout, ctx.Err())
				return
			}
			if errors.Is(err, blob.ErrThrottled) || errors.Is(err, blob.ErrUnavailable) {
				select {
				case <-ctx.Done():
					t.Errorf("cleanup %q exceeded %s: %v", prefix, cleanupTimeout, ctx.Err())
					return
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			t.Errorf("list cleanup objects: %v", err)
			return
		}
		if len(result.Objects) == 0 {
			return
		}
		for _, attributes := range result.Objects {
			if err := store.Delete(ctx, attributes.Key, attributes.Generation); err != nil {
				if ctx.Err() != nil {
					t.Errorf("cleanup %q exceeded %s: %v", prefix, cleanupTimeout, ctx.Err())
					return
				}
				if errors.Is(err, blob.ErrAmbiguous) {
					continue
				}
				t.Errorf("delete cleanup object %q generation %d: %v", attributes.Key, attributes.Generation, err)
				return
			}
		}
	}
}
