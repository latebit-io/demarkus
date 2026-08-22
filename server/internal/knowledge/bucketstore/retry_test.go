package bucketstore

import (
	"testing"
	"time"
)

func TestRetryDelayCapsBaseBeforeAdditiveJitter(t *testing.T) {
	tests := []struct {
		name     string
		attempt  int
		wantBase time.Duration
	}{
		{name: "initial", attempt: 0, wantBase: 100 * time.Millisecond},
		{name: "exponential", attempt: 5, wantBase: 3200 * time.Millisecond},
		{name: "first capped", attempt: 6, wantBase: maximumRetryBaseDelay},
		{name: "saturated", attempt: 11, wantBase: maximumRetryBaseDelay},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var bound int64
			minimum := retryDelay(test.attempt, func(limit int64) int64 {
				bound = limit
				return 0
			})
			maximum := retryDelay(test.attempt, func(limit int64) int64 {
				return limit - 1
			})
			if minimum != test.wantBase {
				t.Errorf("minimum delay = %v, want %v", minimum, test.wantBase)
			}
			wantBound := int64(test.wantBase/2) + 1
			if bound != wantBound || maximum != test.wantBase+time.Duration(wantBound-1) {
				t.Errorf("jitter range = [%v,%v], bound %d", minimum, maximum, bound)
			}
			if maximum == minimum {
				t.Error("additive jitter collapsed")
			}
		})
	}
}
