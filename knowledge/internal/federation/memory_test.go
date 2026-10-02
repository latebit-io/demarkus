package federation

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
)

// agentDoc is a production-shaped body: graph, the agent's export of one
// cycle, plus links that make edges. Each call is its own string.
func agentDoc(graph []byte, cycle, links int) string {
	var body strings.Builder
	body.Grow(len(graph) + links*64)
	body.Write(graph)
	for i := range links {
		fmt.Fprintf(&body, "[n%d](/agents/session-%d/inbox/01HZX%08d.md)\n", i, i%7, cycle*links+i)
	}
	return body.String()
}

// A world's state tracks its live rows: many cycles of agent-shaped edits,
// creates, archives and failed reads, and a burst of reads failing while the
// world was unavailable, leave it holding its rows and nothing more.
func TestWorldRetainsOnlyItsLiveRows(t *testing.T) {
	const live, cycles, edits, links, burst = 16, 100, 4, 32, 5000
	ctx, alpha := context.Background(), newFakeWorld(t)
	d := testDeriver(Config{Source: fakeSource{"alpha": alpha}, Hub: newFakeHub().io()})
	doc := func(i int) string { return fmt.Sprintf("/docs/doc-%d.md", i) }
	run := func(cycle int) string { return fmt.Sprintf("/agents/run-%d.md", cycle) }
	owned := memtest.Owned(func() any {
		w := newWorld(d, "alpha")
		graph := memtest.AgentGraphBody(0)
		publish := func(docPath string, cycle int) {
			applyEvent(t, w, docPath, alpha.publish(docPath, agentDoc(graph, cycle, links)), protocol.OpPublish)
		}
		checkpoint := func(cycle int) {
			if err := w.checkpoint(ctx, protocol.Cursor{Epoch: "e1", Seq: uint64(cycle + 1)}); err != nil {
				t.Fatal(err)
			}
		}
		for i := range live {
			publish(doc(i), 0)
		}
		publish(run(0), 0)
		checkpoint(0)
		for cycle := 1; cycle <= cycles; cycle++ {
			graph = memtest.AgentGraphBody(cycle)
			for i := range edits {
				publish(doc((cycle*edits+i)%live), cycle)
			}
			publish(run(cycle), cycle)
			alpha.archive(run(cycle - 1))
			applyEvent(t, w, run(cycle-1), 1, protocol.OpArchive)
			// One read fails now and succeeds at the checkpoint's retry.
			failing := doc(cycle % live)
			alpha.setFailing(failing, true)
			publish(failing, cycle)
			alpha.setFailing(failing, false)
			if cycle == cycles/2 {
				for i := range burst {
					unavailable := fmt.Sprintf("/agents/burst-%d.md", i)
					alpha.setFailing(unavailable, true)
					applyEvent(t, w, unavailable, 1, protocol.OpPublish)
					alpha.setFailing(unavailable, false)
				}
			}
			checkpoint(cycle)
		}
		if w.sources != live+1 || len(w.retry) != 0 || len(w.dirty) != 0 || !w.complete() {
			t.Fatalf("after the cycles: %d rows, %d retries, %d dirty, complete %t", w.sources, len(w.retry), len(w.dirty), w.complete())
		}
		return w
	})
	runtime.KeepAlive(d) // and the fakes it reads and writes
	t.Logf("the world holds %d bytes after %d cycles", owned, cycles)
	// 17 rows of 32 edges are about 110 KB; a burst's retry buckets kept
	// would add 220 KB. Near zero means something else holds the world.
	if floor, limit := int64(32<<10), int64(192<<10); owned < floor || owned > limit {
		t.Errorf("the world holds %d bytes, want %d to %d", owned, floor, limit)
	}
}
