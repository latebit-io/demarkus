package filestore

import (
	"context"
	"time"
)

// outsidePoll bounds how late a waiting watcher learns of a write made
// outside this process: a watch opens no read view, so nothing else looks at
// the marker for it. The same backstop as the bucket store's follow interval.
const outsidePoll = 5 * time.Second

// watchOutside checks the generation marker on a timer while a watcher waits
// on the hub: a change rebuilds the index and resyncs every watcher.
func (s *Store) watchOutside(interval time.Duration) {
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.stopOutside = func() {
		stop()
		<-done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if s.changes.Waiting() == 0 {
				continue
			}
			if err := s.reindexIfChanged(ctx); err != nil && ctx.Err() == nil {
				s.logger.Warn("outside write check failed", "error", err)
			}
		}
	}()
}
