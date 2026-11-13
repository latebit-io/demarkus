package driftreport

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/memtest"
	"github.com/latebit-io/demarkus/protocol/provenance"
	"github.com/latebit-io/demarkus/protocol/render"
)

// bigWorld is a world of dirs*perDir documents with bodies of bodyBytes: a
// source and the documents pinned to it per directory, half of them owned.
// bump moves every source, so every pinned document drifts further.
type bigWorld struct {
	mu      sync.Mutex
	version int
	body    string
}

const dirs, perDir, bodyBytes = 4, 500, 8 << 10

func (w *bigWorld) bump() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.version++
}

func (w *bigWorld) list(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
	var entries []render.ListEntry
	if dir := strings.Trim(r.Path, "/"); dir == "" {
		for d := range dirs {
			entries = append(entries, render.ListEntry{Name: fmt.Sprintf("d%d", d), IsDir: true})
		}
	} else {
		for i := range perDir {
			entries = append(entries, render.ListEntry{Name: fmt.Sprintf("doc-%03d.md", i)})
		}
	}
	return fetch.Result{Response: render.ListResponse(r.Path, entries, "")}, nil
}

func (w *bigWorld) fetch(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	dir, name, ok := strings.Cut(strings.TrimPrefix(r.Path, "/"), "/")
	if !ok || !strings.HasPrefix(name, "doc-") {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	w.mu.Lock()
	version := w.version
	w.mu.Unlock()
	i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "doc-"), ".md"))
	if err != nil {
		return fetch.Result{}, err
	}
	meta := map[string]string{"tags": "docs"}
	switch {
	case i == 0: // the directory's source
	case i%2 == 0:
		meta[provenance.Key], meta[protocol.MetaUser] = "/"+dir+"/doc-000.md/v1", fmt.Sprintf("user-%d@example.com", i%7)
	default:
		meta[provenance.Key] = "/" + dir + "/doc-000.md/v1"
	}
	if i != 0 {
		version = 1
	}
	return fetchtest.Head(w.body, version, meta), nil
}

// The reporter lives as long as the broker leads: after many passes over a
// production-shaped world it must hold nothing of the worlds it read.
func TestReporterRetainsNothingOfAPass(t *testing.T) {
	const passes = 5
	world := &bigWorld{version: 1, body: "# Doc\n\nSummary.\n\n" + strings.Repeat("word ", bodyBytes/5)}
	store := &fetchtest.Client{ListFn: world.list, FetchFn: world.fetch}
	ctx := context.Background()
	var took time.Duration
	var allocated uint64
	owned := memtest.Owned(func() any {
		// Unpaced: the limiter only stretches the pass.
		r := &reporter{Worlds: []string{"big"}, Store: store}
		for range passes {
			world.bump()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			outcome, err := r.report(ctx, "big")
			took = time.Since(start)
			runtime.ReadMemStats(&after)
			allocated = after.TotalAlloc - before.TotalAlloc
			if want := fmt.Sprintf("published, %d documents drifted", dirs*(perDir-1)); err != nil || outcome != want {
				t.Fatalf("report = %q, %v; want %q", outcome, err, want)
			}
		}
		return r
	})
	runtime.KeepAlive(store)
	t.Logf("%d documents of %d KB: one pass took %s unpaced and allocated %d MB in all; the reporter owns %d bytes after %d passes", dirs*perDir, bodyBytes>>10, took, allocated>>20, owned, passes)
	if owned > 64<<10 {
		t.Errorf("the reporter owns %d bytes after %d passes over %d MB of bodies, want under 64 KB", owned, passes, dirs*perDir*bodyBytes>>20)
	}
}
