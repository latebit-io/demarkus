package generation_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/protocol"
)

// docs is a versioned document store with optimistic concurrency, as a server is.
type docs struct {
	current  map[string]int
	bodies   map[string]string // path and path/vN
	writes   []string
	failOn   string // publish of this path fails after...
	landsOn  bool   // ...the write landed (a lost response) or did not
	failWith error
}

func newDocs() *docs { return &docs{current: map[string]int{}, bodies: map[string]string{}} }

func (d *docs) response(path string) protocol.Response {
	body, ok := d.bodies[path]
	if !ok {
		return protocol.Response{Status: protocol.StatusNotFound}
	}
	version := d.current[strings.SplitN(path, "/v", 2)[0]]
	if i := strings.LastIndex(path, "/v"); i > 0 {
		if n, err := strconv.Atoi(path[i+2:]); err == nil {
			version = n
		}
	}
	return protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: map[string]string{
		"version": strconv.Itoa(version), "content-hash": generation.BodyHash(body),
	}}
}

func (d *docs) io() generation.IO {
	return generation.IO{
		Fetch: func(_ context.Context, path string) (protocol.Response, error) { return d.response(path), nil },
		Publish: func(_ context.Context, path, body string, expected int) (protocol.Response, error) {
			if path == d.failOn && !d.landsOn {
				return protocol.Response{}, d.failWith
			}
			if d.current[path] != expected {
				return protocol.Response{Status: protocol.StatusConflict}, nil
			}
			d.current[path]++
			d.bodies[path] = body
			d.bodies[generation.VersionPath(path, d.current[path])] = body
			d.writes = append(d.writes, path)
			if path == d.failOn {
				return protocol.Response{}, d.failWith
			}
			return protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": strconv.Itoa(d.current[path])}}, nil
		},
	}
}

type ref struct {
	Path    string
	Version int
}

// spec is a toy generation: one shard per word, a manifest that names the slot.
func spec(words ...string) generation.Spec[ref] {
	return generation.Spec[ref]{
		Labels:       generation.Labels{Manifest: "manifest", Shard: "shard"},
		ManifestPath: "/gen.md",
		ActiveSlot: func(body string) (string, error) {
			slot, ok := strings.CutPrefix(strings.SplitN(body, "\n", 2)[0], "slot: ")
			if !ok {
				return "", errors.New("not a manifest")
			}
			return slot, nil
		},
		Shards: func(slot string) ([]generation.Shard[ref], error) {
			shards := make([]generation.Shard[ref], 0, len(words))
			for _, word := range words {
				path := "/gen.shards/" + slot + "/" + word + ".md"
				shards = append(shards, generation.Shard[ref]{
					Artifact: generation.Artifact{Path: path, Body: word, ContentHash: generation.BodyHash(word)},
					Ref:      func(version int) ref { return ref{Path: path, Version: version} },
				})
			}
			return shards, nil
		},
		VerifyShard: func(r ref, resp protocol.Response) error {
			if want := strings.TrimSuffix(r.Path[strings.LastIndex(r.Path, "/")+1:], ".md"); resp.Body != want {
				return fmt.Errorf("shard %s holds %q", r.Path, resp.Body)
			}
			return nil
		},
		Manifest: func(slot string, refs []ref) (string, error) {
			return fmt.Sprintf("slot: %s\nshards: %d\n", slot, len(refs)), nil
		},
	}
}

func TestPublishStagesShardsThenCommitsTheManifestLast(t *testing.T) {
	store := newDocs()
	first, err := generation.Publish(t.Context(), spec("alpha", "beta"), store.io())
	if err != nil {
		t.Fatal(err)
	}
	if first.ShardsPublished != 2 || first.ShardsReused != 0 || first.ManifestVersion != 1 || first.Slot != generation.SlotA {
		t.Errorf("first = %+v", first)
	}
	if got := strings.Join(store.writes, ","); got != "/gen.shards/a/alpha.md,/gen.shards/a/beta.md,/gen.md" {
		t.Errorf("writes = %s, want the manifest last", got)
	}
	if len(first.Refs) != 2 || first.Refs[0] != (ref{Path: "/gen.shards/a/alpha.md", Version: 1}) || first.Manifest.Body != "slot: a\nshards: 2\n" {
		t.Errorf("refs = %+v, manifest = %q", first.Refs, first.Manifest.Body)
	}

	// The next generation stages the other slot, so readers of the live one never see a half written set.
	second, err := generation.Publish(t.Context(), spec("alpha", "beta"), store.io())
	if err != nil || second.Slot != generation.SlotB || second.ManifestVersion != 2 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	// Back on slot a with the same content: the shards are already there.
	third, err := generation.Publish(t.Context(), spec("alpha", "beta"), store.io())
	if err != nil || third.Slot != generation.SlotA || third.ShardsReused != 2 || third.ShardsPublished != 0 {
		t.Fatalf("third = %+v, %v", third, err)
	}
}

func TestPublishRefusesAStaleManifestVersion(t *testing.T) {
	store := newDocs()
	if _, err := generation.Publish(t.Context(), spec("alpha"), store.io()); err != nil {
		t.Fatal(err)
	}
	writes := len(store.writes)
	stale := spec("gamma")
	stale.ExpectedManifestVersion = new(0)
	_, err := generation.Publish(t.Context(), stale, store.io())
	if err == nil || err.Error() != "manifest version changed: got 1, expected 0" || len(store.writes) != writes {
		t.Errorf("err = %v, writes %d -> %d, want a refusal before any write", err, writes, len(store.writes))
	}
}

func TestPublishReconcilesALostResponseAndStopsOnARealFailure(t *testing.T) {
	lost := errors.New("response lost")
	landed := newDocs()
	landed.failOn, landed.landsOn, landed.failWith = "/gen.shards/a/alpha.md", true, lost
	got, err := generation.Publish(t.Context(), spec("alpha"), landed.io())
	if err != nil || got.ShardsPublished != 1 || got.ManifestVersion != 1 {
		t.Fatalf("landed write = %+v, %v, want it found at the head and the generation committed", got, err)
	}

	failed := newDocs()
	failed.failOn, failed.failWith = "/gen.shards/a/alpha.md", lost
	_, err = generation.Publish(t.Context(), spec("alpha", "beta"), failed.io())
	if !errors.Is(err, lost) || !strings.Contains(err.Error(), "reconcile:") {
		t.Fatalf("err = %v, want the cause and the failed reconcile", err)
	}
	if len(failed.writes) != 0 {
		t.Errorf("writes = %v, want nothing after the failed shard, the manifest least of all", failed.writes)
	}
}
