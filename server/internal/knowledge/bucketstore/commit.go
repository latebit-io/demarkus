package bucketstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/internal/backend"
)

// runMutation commits one mutation, then runs the Committed hook with the
// commit token released: a hook that reenters the store must not deadlock.
func (store *Store) runMutation(ctx context.Context, build mutationBuilder) (mutationResult, error) {
	result, err := store.commitMutation(ctx, build)
	if err == nil && result.Sequence != 0 && store.committed != nil {
		store.committed(result.Sequence)
	}
	return result, err
}

// commitMutation builds the mutation on the log's tip and creates the next
// slot; a slot lost to another writer rebuilds it on the new tip, until the
// request deadline.
func (store *Store) commitMutation(ctx context.Context, build mutationBuilder) (mutationResult, error) {
	if store.readOnly {
		return mutationResult{}, backend.ErrReadOnly
	}
	ctx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	select {
	case <-store.commitToken:
		defer func() { store.commitToken <- struct{}{} }()
	case <-ctx.Done():
		return mutationResult{}, fmt.Errorf("wait for commit token: %w", ctx.Err())
	}
	// Checked with the token held: Close takes the token, so a commit that
	// waited across it is refused rather than landing after it returned.
	if store.closed.Load() {
		return mutationResult{}, backend.ErrClosed
	}

	operationID, err := store.newOperationID()
	if err != nil {
		return mutationResult{}, fmt.Errorf("create operation ID: %w", err)
	}
	if !validWorldID(operationID) {
		return mutationResult{}, fmt.Errorf("create operation ID: invalid UUID %q", operationID)
	}

	for {
		base, err := store.refresh(ctx)
		if err != nil {
			return mutationResult{}, fmt.Errorf("operation %s refresh: %w", operationID, err)
		}
		candidate, built, err := build(ctx, &readView{objects: store.objects, snapshot: base}, operationID)
		if err != nil || candidate == nil {
			return built, err
		}
		sequence, err := store.commitSlot(ctx, base, candidate)
		if err != nil {
			return built, fmt.Errorf("operation %s commit: %w", operationID, err)
		}
		if sequence != 0 {
			built.Sequence = sequence
			return built, nil
		}
	}
}

// commitSlot stages the candidate's objects, applies its slot to a copy of
// base, and creates the slot; it reports the committed sequence, or 0 when
// another writer took the name. A slot that does not apply is never created.
func (store *Store) commitSlot(ctx context.Context, base *snapshot, candidate *candidateMutation) (int64, error) {
	if err := runParallel(ctx, store.shardWorkers, candidate.objects, func(ctx context.Context, object modelObject) error {
		return createImmutable(ctx, store.objects, object)
	}); err != nil {
		return 0, fmt.Errorf("stage: %w", err)
	}
	slot := &slotObject{
		Schema: logSchema, WorldID: store.worldID, First: base.Sequence + 1, Store: store.id, Prev: base.Tip,
		Entries: []slotEntry{candidate.entry},
	}
	data, err := marshalImmutable(slot)
	if err == nil {
		err = validateSlot(slot, slot.First)
	}
	if err != nil {
		return 0, fmt.Errorf("build slot: %w", err)
	}
	store.refreshMu.Lock()
	next := base.derive()
	store.refreshMu.Unlock()
	reindex := make(map[string]struct{})
	if err := next.applySlot(slot, hashHex(data), reindex); err != nil {
		return 0, fmt.Errorf("slot %d would not apply: %w", slot.First, err)
	}
	var fresh map[string][]byte
	if candidate.body != nil {
		fresh = map[string][]byte{candidate.entry.Path: candidate.body}
	}
	if err := store.indexSections(ctx, next, reindex, fresh); err != nil {
		return 0, err
	}
	created := store.now()
	err = createImmutable(ctx, store.objects, modelObject{Key: slotKey(slot.First), Data: data})
	switch {
	case errors.Is(err, errNameTaken):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("create slot %d: %w", slot.First, err)
	}
	store.refreshMu.Lock()
	// A refresh that read the slot first has installed it already.
	if store.served.Load().snap.Sequence == base.Sequence {
		store.install(next, []*slotObject{slot}, created)
	}
	store.refreshMu.Unlock()
	return slot.First, nil
}

func randomOperationID() (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:]), nil
}
