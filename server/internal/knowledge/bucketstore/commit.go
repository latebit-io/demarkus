package bucketstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

const maximumMutationAttempts = 12

type commitOutcome int

const (
	commitSucceeded commitOutcome = iota
	commitRebase
)

func (store *Store) runMutation(build mutationBuilder) (MutationResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), store.requestTimeout)
	defer cancel()
	select {
	case <-store.commitToken:
		defer func() { store.commitToken <- struct{}{} }()
	case <-ctx.Done():
		return MutationResult{}, ctx.Err()
	}

	operationID, err := store.newOperationID()
	if err != nil {
		return MutationResult{}, fmt.Errorf("create operation ID: %w", err)
	}
	if !validWorldID(operationID) {
		return MutationResult{}, fmt.Errorf("create operation ID: invalid UUID %q", operationID)
	}

	var result MutationResult
	for attempt := range maximumMutationAttempts {
		loaded, err := store.refreshSnapshot(ctx)
		if err != nil {
			return result, fmt.Errorf("operation %s refresh: %w", operationID, err)
		}
		view := &readView{ctx: ctx, cancel: func() {}, objects: store.objects, snapshot: loaded}
		candidate, built, err := build(ctx, view, operationID)
		result = built
		if err != nil || candidate == nil {
			return result, err
		}
		for _, object := range candidate.objects {
			if err := createImmutable(ctx, store.objects, object); err != nil {
				return result, fmt.Errorf("operation %s stage %q: %w", operationID, object.Key, err)
			}
		}
		outcome, err := store.commitCandidate(ctx, candidate)
		if err != nil {
			return result, fmt.Errorf("operation %s commit: %w", operationID, err)
		}
		if outcome == commitSucceeded {
			return candidate.result, nil
		}
		if attempt == maximumMutationAttempts-1 {
			break
		}
	}
	return result, fmt.Errorf("operation %s exhausted %d rebases", operationID, maximumMutationAttempts)
}

func (store *Store) commitCandidate(ctx context.Context, candidate *candidateMutation) (commitOutcome, error) {
	for attempt := range maximumMutationAttempts {
		if err := store.waitForHeadAttempt(ctx); err != nil {
			return commitRebase, err
		}

		store.refreshMu.Lock()
		cached := store.snapshot.Load()
		store.refreshMu.Unlock()
		if cached == nil || cached.HeadGeneration != candidate.base.HeadGeneration {
			return commitRebase, nil
		}
		attributes, replaceErr := store.objects.Replace(
			ctx,
			headObjectKey,
			candidate.base.HeadGeneration,
			candidate.headData,
		)
		if replaceErr == nil {
			store.installCandidateIfBase(candidate, attributes)
			return commitSucceeded, nil
		}

		if errors.Is(replaceErr, blob.ErrPrecondition) {
			return commitRebase, nil
		}
		if !retryableObjectError(replaceErr) {
			return commitRebase, replaceErr
		}

		latest, latestAttributes, err := store.reconcileHead(ctx, candidate.operationID)
		if err != nil {
			return commitRebase, errors.Join(replaceErr, err)
		}
		if receiptCommitted(&latest, candidate.operationID) {
			if latest.Sequence == candidate.head.Sequence && latest.Root == candidate.head.Root {
				store.installCandidateIfBase(candidate, latestAttributes)
			}
			return commitSucceeded, nil
		}
		if latestAttributes.Generation == candidate.base.HeadGeneration {
			if attempt == maximumMutationAttempts-1 {
				return commitRebase, fmt.Errorf("identical head retry exhausted after ambiguous replace")
			}
			continue
		}
		if latest.Sequence < candidate.head.Sequence {
			return commitRebase, fmt.Errorf("%w: head sequence %d is behind candidate %d", blob.ErrIntegrity, latest.Sequence, candidate.head.Sequence)
		}
		if !receiptWindowCovers(&latest, candidate.head.Sequence) {
			return commitRebase, fmt.Errorf("ambiguous outcome: receipt for sequence %d was evicted at head sequence %d", candidate.head.Sequence, latest.Sequence)
		}
		return commitRebase, nil
	}
	return commitRebase, fmt.Errorf("head commit exhausted without result")
}

func (store *Store) reconcileHead(ctx context.Context, operationID string) (headObject, blob.Attributes, error) {
	var lastErr error
	for attempt := range maximumMutationAttempts {
		head, attributes, err := loadHeadObject(ctx, store.objects, store.worldID)
		if err == nil {
			return head, attributes, nil
		}
		lastErr = err
		if !retryableObjectError(err) || attempt == maximumMutationAttempts-1 {
			break
		}
		if err := waitForRetry(ctx, createRetryDelay(attempt)); err != nil {
			return headObject{}, blob.Attributes{}, fmt.Errorf("reconcile operation %s: %w", operationID, errors.Join(lastErr, err))
		}
	}
	return headObject{}, blob.Attributes{}, fmt.Errorf("reconcile operation %s: %w", operationID, lastErr)
}

func (store *Store) installCandidateIfBase(candidate *candidateMutation, attributes blob.Attributes) {
	store.refreshMu.Lock()
	defer store.refreshMu.Unlock()
	cached := store.snapshot.Load()
	if cached == nil || cached.HeadGeneration != candidate.base.HeadGeneration {
		return
	}
	prepared := candidate.prepared
	prepared.HeadAttributes = attributes
	prepared.HeadGeneration = attributes.Generation
	store.snapshot.Store(prepared)
}

func (store *Store) waitForHeadAttempt(ctx context.Context) error {
	if store.commitInterval <= 0 || store.lastHeadTry.IsZero() {
		store.lastHeadTry = store.now()
		return ctx.Err()
	}
	delay := store.lastHeadTry.Add(store.commitInterval).Sub(store.now())
	if delay > 0 {
		if err := waitForRetry(ctx, delay); err != nil {
			return err
		}
	}
	store.lastHeadTry = store.now()
	return ctx.Err()
}

func receiptCommitted(head *headObject, operationID string) bool {
	for _, receipt := range head.Receipts {
		if receipt.OperationID == operationID && receipt.Result == "committed" {
			return true
		}
	}
	return false
}

func receiptWindowCovers(head *headObject, sequence int64) bool {
	if len(head.Receipts) == 0 {
		return false
	}
	return sequence >= head.Receipts[0].Sequence && sequence <= head.Receipts[len(head.Receipts)-1].Sequence
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
