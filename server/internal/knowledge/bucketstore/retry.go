package bucketstore

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"time"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

const maximumCreateAttempts = 12

func retryableObjectError(err error) bool {
	return errors.Is(err, blob.ErrAmbiguous) || errors.Is(err, blob.ErrThrottled) || errors.Is(err, blob.ErrUnavailable)
}

func createRetryDelay(attempt int) time.Duration {
	delay := 100 * time.Millisecond * time.Duration(1<<min(attempt, 6))
	jitter := time.Duration(mathrand.Int64N(int64(delay/2) + 1))
	return min(delay+jitter, 5*time.Second)
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
