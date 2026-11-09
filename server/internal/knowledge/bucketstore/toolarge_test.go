package bucketstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/server/blob"
)

func smallMemory(t *testing.T) *observedBlobStore {
	t.Helper()
	objects, err := blob.NewMemory(64)
	if err != nil {
		t.Fatalf("new memory: %v", err)
	}
	return newObservedBlobStore(objects)
}

// An object over the limit was never written: the create fails at once,
// with no read back to tell a taken name from a refusal.
func TestTooLargeCreatesFailWithoutReadingBack(t *testing.T) {
	big := modelObject{Key: blobKey("abc"), Data: []byte(strings.Repeat("x", 65))}
	tests := []struct {
		name   string
		create func(objects blob.Store) error
	}{
		{name: "content", create: func(objects blob.Store) error {
			return createContent(context.Background(), objects, big)
		}},
		{name: "immutable", create: func(objects blob.Store) error {
			_, err := createOrRead(context.Background(), objects, big, false)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := smallMemory(t)
			if err := test.create(objects); !errors.Is(err, blob.ErrTooLarge) {
				t.Errorf("create = %v, want ErrTooLarge", err)
			}
			counts := objects.counts()
			if n := sumCounts(counts.heads) + sumCounts(counts.gets); n != 0 {
				t.Errorf("%d read backs, want none", n)
			}
		})
	}
}

// A segment part the bucket refuses is reported, not taken for a racer's.
func TestTooLargeSegmentPartIsReported(t *testing.T) {
	objects := smallMemory(t)
	bodies := []segmentBody{{Path: "/a.md", BodyHash: "h", Body: strings.Repeat("x", 100)}}
	if err := writeSegment(context.Background(), objects, segmentRef{label: "0", window: 1}, bodies); !errors.Is(err, blob.ErrTooLarge) {
		t.Errorf("writeSegment = %v, want ErrTooLarge", err)
	}
}
