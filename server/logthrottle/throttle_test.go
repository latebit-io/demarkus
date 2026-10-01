package logthrottle

import (
	"sync"
	"testing"
	"time"
)

func TestThrottleAllow(t *testing.T) {
	type call struct {
		at             time.Duration // since the first call
		wantLog        bool
		wantSuppressed int64
	}
	tests := []struct {
		name  string
		calls []call
	}{
		{
			name:  "first occurrence logs",
			calls: []call{{at: 0, wantLog: true}},
		},
		{
			name: "occurrences inside the window are held back",
			calls: []call{
				{at: 0, wantLog: true},
				{at: time.Second},
				{at: 59 * time.Second},
			},
		},
		{
			name: "first occurrence after the window logs with the held count",
			calls: []call{
				{at: 0, wantLog: true},
				{at: time.Second},
				{at: 2 * time.Second},
				{at: time.Minute, wantLog: true, wantSuppressed: 2},
				{at: time.Minute + time.Second},
				{at: 3 * time.Minute, wantLog: true, wantSuppressed: 1},
			},
		},
		{
			name: "a quiet window resets the count",
			calls: []call{
				{at: 0, wantLog: true},
				{at: 2 * time.Minute, wantLog: true},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			var now time.Time
			throttle := New(time.Minute)
			throttle.now = func() time.Time { return now }
			for i, c := range tt.calls {
				now = start.Add(c.at)
				log, suppressed := throttle.Allow()
				if log != c.wantLog || suppressed != c.wantSuppressed {
					t.Fatalf("call %d at +%s: Allow() = (%v, %d), want (%v, %d)",
						i, c.at, log, suppressed, c.wantLog, c.wantSuppressed)
				}
			}
		})
	}
}

func TestThrottleNilLogsEveryOccurrence(t *testing.T) {
	var throttle *Throttle
	for i := range 3 {
		if log, suppressed := throttle.Allow(); !log || suppressed != 0 {
			t.Fatalf("call %d: Allow() = (%v, %d), want (true, 0)", i, log, suppressed)
		}
	}
}

// A flood from many goroutines logs once per window and loses no count.
func TestThrottleConcurrentCountsEveryOccurrence(t *testing.T) {
	throttle := New(time.Hour)
	const workers, perWorker = 8, 500
	var mu sync.Mutex
	var logged, held int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range perWorker {
				log, _ := throttle.Allow()
				mu.Lock()
				if log {
					logged++
				} else {
					held++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if logged != 1 || held != workers*perWorker-1 {
		t.Fatalf("logged %d, held %d; want 1 and %d", logged, held, workers*perWorker-1)
	}
	throttle.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if log, suppressed := throttle.Allow(); !log || suppressed != workers*perWorker-1 {
		t.Fatalf("after the window: Allow() = (%v, %d), want (true, %d)", log, suppressed, workers*perWorker-1)
	}
}
