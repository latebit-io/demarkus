package bucketstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// openFollower is a store that follows peers on a fast backstop timer.
func openFollower(t *testing.T, objects blob.Store) *Store {
	t.Helper()
	store, err := Open(context.Background(), objects, Options{
		Logger: discardLogger, WorldID: testWorldID, ChangeRing: changefeed.DefaultRingSize, followInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return store
}

// peerCommit publishes through a second store over the same bucket and
// returns the sequence it committed.
func peerCommit(t *testing.T, objects blob.Store, path string) int64 {
	t.Helper()
	peer, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Publish(context.Background(), backend.WriteRequest{Path: path, ExpectedVersion: -1, Content: []byte("# Peer\n")}); err != nil {
		t.Fatal(err)
	}
	return peer.servedSequence()
}

func waitServed(t *testing.T, store *Store, sequence int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for store.servedSequence() < sequence {
		if time.Now().After(deadline) {
			t.Fatalf("served sequence = %d, want %d", store.servedSequence(), sequence)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The backstop timer reads the bucket only while the hub has a subscriber:
// with nobody watching, a peer's commit waits for the next read.
func TestFollowerPollsOnlyForSubscribers(t *testing.T) {
	objects := initializedMemory(t)
	store := openFollower(t, objects)
	sequence := peerCommit(t, objects, "/docs/unwatched.md")

	time.Sleep(100 * time.Millisecond) // ten backstop periods
	if got := store.servedSequence(); got >= sequence {
		t.Fatalf("served sequence = %d with no subscriber, want below %d", got, sequence)
	}
	sub, err := store.Changes().Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	waitServed(t, store, sequence)
	sub.Close()
	if got := store.Changes().Subscribers(); got != 0 {
		t.Errorf("subscribers = %d after close, want 0", got)
	}
}

// A peer's hint reads at once, with nobody watching.
func TestFollowReadsAtOnce(t *testing.T) {
	objects := initializedMemory(t)
	store := openFollower(t, objects)
	sequence := peerCommit(t, objects, "/docs/hinted.md")
	store.Follow(sequence)
	waitServed(t, store, sequence)
}

// Close stops the follow loop and ends the hub's watches; a later hint is
// a no-op rather than a send nobody receives.
func TestCloseStopsFollowing(t *testing.T) {
	objects := initializedMemory(t)
	store, err := Open(context.Background(), objects, Options{Logger: discardLogger, WorldID: testWorldID, ChangeRing: changefeed.DefaultRingSize})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := store.Changes().Subscribe(t.Context(), "/", protocol.Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-store.follower.done:
	default:
		t.Fatal("follow loop still running after Close")
	}
	if _, err := sub.Next(t.Context()); !errors.Is(err, changefeed.ErrClosed) {
		t.Errorf("Next after Close = %v, want ErrClosed", err)
	}
	store.Follow(store.servedSequence() + 1)
	if err := store.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

// Without WATCH there is nothing to follow for: no loop, and Close is free.
func TestStoreWithoutWatchDoesNotFollow(t *testing.T) {
	store, err := Open(context.Background(), initializedMemory(t), Options{Logger: discardLogger, WorldID: testWorldID})
	if err != nil {
		t.Fatal(err)
	}
	if store.follower != nil {
		t.Fatal("a store without WATCH started a follow loop")
	}
	store.Follow(store.servedSequence() + 1)
	if err := store.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}
