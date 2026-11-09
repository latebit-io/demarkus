package filestore

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// maxReaders caps concurrent readers; Lock takes all of them.
const maxReaders = 1 << 30

// ctxRWMutex is a read-write lock whose waits honor a context. FIFO, so a
// waiting writer holds back later readers as sync.RWMutex does.
type ctxRWMutex struct{ sem *semaphore.Weighted }

func newCtxRWMutex() ctxRWMutex { return ctxRWMutex{sem: semaphore.NewWeighted(maxReaders)} }

// Lock and RLock hold nothing and return ctx's error when ctx ends first;
// Acquire checks ctx again after a wait, so a canceled waiter never runs.
func (m ctxRWMutex) Lock(ctx context.Context) error  { return m.sem.Acquire(ctx, maxReaders) }
func (m ctxRWMutex) RLock(ctx context.Context) error { return m.sem.Acquire(ctx, 1) }
func (m ctxRWMutex) Unlock()                         { m.sem.Release(maxReaders) }
func (m ctxRWMutex) RUnlock()                        { m.sem.Release(1) }
func (m ctxRWMutex) TryLock() bool                   { return m.sem.TryAcquire(maxReaders) }
