package federation

import (
	"errors"
	"maps"
	"slices"
	"sync/atomic"
	"testing"
)

func TestGateCheckRefusesAGatedWorldIntoAnOpenHub(t *testing.T) {
	gated := map[string]bool{"private": true, "gated-hub": true}
	isGated := func(world string) bool { return gated[world] }
	open := GateCheck("hub", isGated)
	if err := open("private"); !errors.Is(err, ErrGatedIntoOpenHub) {
		t.Errorf("gated world, open hub: err = %v, want ErrGatedIntoOpenHub", err)
	}
	if err := open("public"); err != nil {
		t.Errorf("open world, open hub: %v", err)
	}
	// A gated hub is read by the readers the gated world admits.
	closed := GateCheck("gated-hub", isGated)
	for _, world := range []string{"private", "public"} {
		if err := closed(world); err != nil {
			t.Errorf("%s into a gated hub: %v", world, err)
		}
	}
}

// The deriver reads in process, past a world's gate: a gated world must not
// be read or checkpointed into a hub anonymous readers can fetch.
func TestDeriverLeavesARefusedWorldUnreadAndUncheckpointed(t *testing.T) {
	h := newHarness(t)
	h.alpha.publish("/index.md", "# Private\n\n[a](a.md)\n")
	h.alpha.publish("/a.md", "# A\n")
	var refuse atomic.Bool
	refuse.Store(true)
	cfg := Config{
		Worlds: slices.Collect(maps.Keys(h.worlds)), Source: h.worlds, Hub: h.hub.io(),
		Refuse: func(string) error {
			if refuse.Load() {
				return ErrGatedIntoOpenHub
			}
			return nil
		},
	}
	stop := runDeriver(t, testDeriver(cfg))
	quiet()
	if reads := h.alpha.reads("/index.md"); reads != 0 {
		t.Fatalf("a refused world was read %d times", reads)
	}
	if _, _, err := h.checkpoint(); err == nil {
		t.Fatal("a refused world has a checkpoint in the hub")
	}

	// The gate lifts (the hub was gated): the world is derived.
	refuse.Store(false)
	h.await(versions(map[string]int{"/index.md": 1, "/a.md": 1}))

	// And closes again under the running loop: nothing more is written.
	refuse.Store(true)
	mark := h.hub.mark()
	h.alpha.publish("/later.md", "# Later\n")
	quiet()
	if got := h.hub.written(mark); len(got) != 0 {
		t.Fatalf("hub written at %v after the world was refused", got)
	}
	stop()
}
