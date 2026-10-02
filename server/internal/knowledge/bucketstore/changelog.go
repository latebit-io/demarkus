package bucketstore

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// changeBlockSize is half the receipt window, so the receipts of nine
// consecutive heads cover each block and a seal lost with its writer is
// completed by the commits after it.
const changeBlockSize = maximumReceipts / 2

// changeBlock is the durable record of one run of receipts past the head's
// window. Its bytes follow from the receipts alone, so every writer that
// seals it writes the same object.
type changeBlock struct {
	Schema   int                `json:"schema"`
	WorldID  string             `json:"world_id"`
	First    int64              `json:"first"`
	Receipts []operationReceipt `json:"receipts"`
}

func changeBlockKey(block int64) string {
	return fmt.Sprintf("%schanges/%016x.json", objectPrefix, block)
}

// blockOf is the block holding sequence.
func blockOf(sequence int64) int64 { return (sequence - 1) / changeBlockSize }

// blockRange is the sequences block holds; sequence 1 creates the world and
// has no receipt.
func blockRange(block int64) (first, last int64) {
	return max(block*changeBlockSize+1, 2), (block + 1) * changeBlockSize
}

func validateChangeBlock(block *changeBlock, worldID string, index int64) error {
	if block.Schema != schemaVersion {
		return fmt.Errorf("schema is %d, want %d", block.Schema, schemaVersion)
	}
	if block.WorldID != worldID {
		return fmt.Errorf("world ID %q, want %q", block.WorldID, worldID)
	}
	first, last := blockRange(index)
	if block.First != first || int64(len(block.Receipts)) != last-first+1 {
		return fmt.Errorf("holds %d receipts from %d, want %d..%d", len(block.Receipts), block.First, first, last)
	}
	return validateReceipts(block.Receipts, first)
}

// sealChanges writes every complete block the served head's receipts cover
// and this store has not sealed. A failure is logged, not returned: the
// commit stands, and the commits after it cover the same block.
func (store *Store) sealChanges(ctx context.Context) {
	snap := store.snapshot.Load()
	if snap == nil || len(snap.Head.Receipts) == 0 {
		return
	}
	head := &snap.Head
	oldest := head.Receipts[0].Sequence
	for block := blockOf(oldest); ; block++ {
		first, last := blockRange(block)
		if last > head.Sequence {
			return
		}
		if first < oldest || last <= store.sealedThrough.Load() {
			continue
		}
		data, err := marshalImmutable(changeBlock{
			Schema:   schemaVersion,
			WorldID:  store.worldID,
			First:    first,
			Receipts: head.Receipts[first-oldest : last-oldest+1],
		})
		if err == nil {
			err = createImmutable(ctx, store.objects, modelObject{Key: changeBlockKey(block), Data: data})
		}
		if err != nil {
			store.logger.Warn("change block not sealed; a later commit retries", "world", store.worldID, "block", block, "error", err)
			return
		}
		for {
			sealed := store.sealedThrough.Load()
			if sealed >= last || store.sealedThrough.CompareAndSwap(sealed, last) {
				break
			}
		}
	}
}

// changeLog is the world's sealed blocks as the hub's backlog. Blocks are
// immutable, so a ring's worth of recently read ones stay cached (LRU): a
// burst of resumes, honest or not, costs the bucket one read per block.
type changeLog struct {
	objects blob.Store
	worldID string
	workers int
	logger  *slog.Logger
	store   *Store

	mu     sync.Mutex
	limit  int
	recent *list.List // front is the most recently used *cachedBlock
	blocks map[int64]*list.Element
}

type cachedBlock struct {
	index int64
	block changeBlock
}

var _ changefeed.Backlog = (*changeLog)(nil)

func newChangeLog(store *Store, ring int) *changeLog {
	return &changeLog{
		objects: store.objects,
		worldID: store.worldID,
		workers: store.shardWorkers,
		logger:  store.logger,
		store:   store,
		limit:   max(ring/changeBlockSize, 1),
		recent:  list.New(),
		blocks:  make(map[int64]*list.Element),
	}
}

// CatchUp polls the bucket head, which reports what peers committed.
func (backlog *changeLog) CatchUp(ctx context.Context) error { return backlog.store.poll(ctx) }

func (backlog *changeLog) cached(index int64) (changeBlock, bool) {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	element, ok := backlog.blocks[index]
	if !ok {
		return changeBlock{}, false
	}
	backlog.recent.MoveToFront(element)
	return element.Value.(*cachedBlock).block, true
}

func (backlog *changeLog) remember(index int64, block changeBlock) {
	backlog.mu.Lock()
	defer backlog.mu.Unlock()
	if element, ok := backlog.blocks[index]; ok {
		backlog.recent.MoveToFront(element)
		return
	}
	backlog.blocks[index] = backlog.recent.PushFront(&cachedBlock{index: index, block: block})
	for backlog.recent.Len() > backlog.limit {
		oldest := backlog.recent.Back()
		delete(backlog.blocks, oldest.Value.(*cachedBlock).index)
		backlog.recent.Remove(oldest)
	}
}

// Events reads the blocks holding (after, through]. A block not yet sealed,
// or a receipt that names no change, fails the read: the watcher resyncs.
func (backlog *changeLog) Events(ctx context.Context, after, through uint64) ([]changefeed.Event, error) {
	if through <= after {
		return nil, nil
	}
	first, err := headSequence(after + 1)
	if err != nil {
		return nil, err
	}
	last, err := headSequence(through)
	if err != nil {
		return nil, err
	}
	firstBlock, lastBlock := blockOf(first), blockOf(last)
	blocks := make([]changeBlock, lastBlock-firstBlock+1)
	var missing []int64
	for index := firstBlock; index <= lastBlock; index++ {
		if block, ok := backlog.cached(index); ok {
			blocks[index-firstBlock] = block
		} else {
			missing = append(missing, index)
		}
	}
	err = runParallel(ctx, backlog.workers, missing, func(ctx context.Context, index int64) error {
		block, err := backlog.load(ctx, index)
		if err != nil {
			return err
		}
		backlog.remember(index, block)
		blocks[index-firstBlock] = block
		return nil
	})
	if err != nil {
		level := slog.LevelWarn
		switch {
		case errors.Is(err, blob.ErrNotFound):
			level = slog.LevelInfo
		case errors.Is(err, blob.ErrIntegrity):
			level = slog.LevelError
		}
		backlog.logger.Log(ctx, level, "change backlog unavailable; watcher resyncs", "world", backlog.worldID, "after", after, "through", through, "error", err)
		return nil, err
	}
	events := make([]changefeed.Event, 0, through-after)
	for i := range blocks {
		for j := range blocks[i].Receipts {
			receipt := &blocks[i].Receipts[j]
			seq := hubSeq(receipt.Sequence)
			if seq <= after || seq > through {
				continue
			}
			if receipt.Path == "" {
				return nil, fmt.Errorf("sequence %d was committed before receipts named their change", seq)
			}
			events = append(events, receiptEvent(receipt))
		}
	}
	return events, nil
}

func (backlog *changeLog) load(ctx context.Context, index int64) (changeBlock, error) {
	block, _, err := getValidated(ctx, backlog.objects, changeBlockKey(index), func(block *changeBlock) error {
		return validateChangeBlock(block, backlog.worldID, index)
	})
	return block, err
}

// headSequence is a hub sequence as a head sequence, the inverse of hubSeq.
func headSequence(seq uint64) (int64, error) {
	if seq > math.MaxInt64 {
		return 0, fmt.Errorf("sequence %d is past any head", seq)
	}
	return int64(seq), nil
}
