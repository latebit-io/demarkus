package gcs

import (
	"context"
	"sync"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/server/blob"
)

// Lazy opens bucket stores over one GCS client built on first use, so a
// process that never reaches a bucket needs no credentials. A failed build
// is retried on the next Open.
type Lazy struct {
	mu     sync.Mutex
	client *storage.Client
}

// Open is the bucket store for bucket, building the client under the
// caller's context the first time.
func (l *Lazy) Open(ctx context.Context, bucket string, maxObjectBytes int64) (blob.Store, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, err
		}
		l.client = client
	}
	return New(l.client, bucket, maxObjectBytes)
}

// Opened reports whether a client was ever built.
func (l *Lazy) Opened() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.client != nil
}

// Close releases the client, if one was built.
func (l *Lazy) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.client == nil {
		return nil
	}
	err := l.client.Close()
	l.client = nil
	return err
}
