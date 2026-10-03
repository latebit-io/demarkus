package bucketstore

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// The log alone determines the world: a replica that replays every slot from
// checkpoint zero, one that followed by refreshing, and the writers that
// installed their own commits all hold the same snapshot.
func TestReplayFromCheckpointZeroEqualsLiveSnapshot(t *testing.T) {
	objects := initializedMemory(t)
	site := &bucketSite{objects: objects}
	writer, peer, follower := site.open(t, 0), site.open(t, 0), site.open(t, 0)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for index, path := range []string{"/a.md", "/docs/b.md", "/docs/deep/c.md", "/.hidden/d.md", "/docs/versions/e.md"} {
		_, err := writer.WriteVersion(path, 0, fmt.Appendf(nil, "# %d\n\n## Part\n\nbody %d\n", index, index), map[string]string{"tags": "replay"})
		must(err)
	}
	_, err := peer.WriteVersion("/docs/b.md", 1, []byte("# B two\n"), nil)
	must(err)
	_, err = writer.AppendVersion("/docs/b.md", 2, []byte("more\n"), nil)
	must(err)
	_, _, err = peer.ArchiveResult("/a.md", true)
	must(err)
	_, _, err = writer.ArchiveResult("/docs/deep/c.md", true)
	must(err)
	_, _, err = writer.ArchiveResult("/docs/deep/c.md", false)
	must(err)
	for version := 1; version <= 6; version++ {
		_, err := peer.WriteVersion("/retained.md", version-1, fmt.Appendf(nil, "v%d", version), map[string]string{"retention": "3"})
		must(err)
	}
	_, err = writer.WriteVersion("/docs/same.md", 0, []byte("# 0\n\n## Part\n\nbody 0\n"), nil)
	must(err)
	must(writer.poll(ctx))
	must(peer.poll(ctx))
	must(follower.poll(ctx))

	live := snapshotDigest(writer.served.Load().snap)
	replayed := snapshotDigest(site.open(t, 0).served.Load().snap)
	for name, other := range map[string]digest{
		"replayed from checkpoint zero": replayed,
		"the other writer":              snapshotDigest(peer.served.Load().snap),
		"a follower":                    snapshotDigest(follower.served.Load().snap),
	} {
		if !reflect.DeepEqual(live, other) {
			t.Errorf("%s differs from the live snapshot:\nlive  %+v\nother %+v", name, live, other)
		}
	}
	if live.sequence != 18 {
		t.Errorf("log tip = %d, want 18", live.sequence)
	}
}

// digest is a snapshot's content without its pointers: the same world gives
// the same digest however the snapshot was built.
type digest struct {
	sequence int64
	tip      string
	paths    []string
	children []dirChild
	hashes   []hashEntry
}

func snapshotDigest(snap *snapshot) digest {
	d := digest{sequence: snap.Sequence, tip: snap.Tip}
	snap.Paths.Ascend(func(state *pathState) bool {
		versions := make([]string, 0, len(state.Recent))
		for _, version := range state.Recent {
			versions = append(versions, fmt.Sprintf("%d:%s:%s:%s", version.entry.Version, version.entry.Blob.Hash, version.entry.BodyHash, version.modified))
		}
		d.paths = append(d.paths, fmt.Sprintf("%s current=%d first=%d archived=%t body=%s modified=%s base=%t entry=%+v versions=%v",
			state.Path, state.Current, state.First, state.Archived, state.BodyHash, state.Modified, state.Base != nil,
			*state.Entry, versions))
		return true
	})
	snap.Children.Ascend(func(child *dirChild) bool {
		d.children = append(d.children, *child)
		return true
	})
	snap.Hashes.Ascend(func(entry hashEntry) bool {
		d.hashes = append(d.hashes, entry)
		return true
	})
	return d
}
