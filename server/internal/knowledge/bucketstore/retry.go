package bucketstore

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"time"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

const maximumCreateAttempts = 12

const maximumRetryBaseDelay = 5 * time.Second

func retryableObjectError(err error) bool {
	return errors.Is(err, blob.ErrAmbiguous) || errors.Is(err, blob.ErrThrottled) || errors.Is(err, blob.ErrUnavailable)
}

func createRetryDelay(attempt int) time.Duration {
	return retryDelay(attempt, mathrand.Int64N)
}

func retryDelay(attempt int, random func(int64) int64) time.Duration {
	base := min(100*time.Millisecond*time.Duration(1<<min(attempt, 6)), maximumRetryBaseDelay)
	jitter := time.Duration(random(int64(base/2) + 1))
	return base + jitter
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
