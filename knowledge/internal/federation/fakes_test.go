package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/generation"
	"github.com/latebit-io/demarkus/client/graphstore"
	"github.com/latebit-io/demarkus/protocol"
)

// fakeDoc is one document of a fakeWorld at its head version.
type fakeDoc struct {
	version  int
	body     string
	archived bool
	private  bool // refused to an anonymous reader
}

// fakeWorld is one world: documents, a change feed with a cursor per change,
// and the in-process watches following it.
type fakeWorld struct {
	t       *testing.T
	mu      sync.Mutex
	epoch   string
	docs    map[string]*fakeDoc
	log     []protocol.WatchEvent
	changed chan struct{} // closed and replaced on every change
	streams []net.Conn    // the world's end of each open watch
	fetches map[string]int
	lists   int
	failing map[string]bool // fetches that fail for now
}

func newFakeWorld(t *testing.T) *fakeWorld {
	return &fakeWorld{t: t, epoch: "e1", docs: map[string]*fakeDoc{}, changed: make(chan struct{}), fetches: map[string]int{}, failing: map[string]bool{}}
}

// publish writes body to docPath as its next version, announces it and
// returns the version.
func (f *fakeWorld) publish(docPath, body string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc := f.docs[docPath]
	if doc == nil {
		doc = &fakeDoc{}
		f.docs[docPath] = doc
	}
	doc.version++
	doc.body = body
	f.emit(docPath, doc.version, protocol.OpPublish)
	return doc.version
}

// archive archives docPath, keeping its version, and announces it.
func (f *fakeWorld) archive(docPath string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	doc := f.docs[docPath]
	doc.archived = true
	f.emit(docPath, doc.version, protocol.OpArchive)
}

func (f *fakeWorld) emit(docPath string, version int, op string) {
	f.log = append(f.log, protocol.WatchEvent{Cursor: f.cursorAt(len(f.log) + 1), Path: docPath, Version: version, Op: op})
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeWorld) cursorAt(seq int) protocol.Cursor {
	return protocol.Cursor{Epoch: f.epoch, Seq: uint64(seq)}
}

// newEpoch forgets the feed, as a store restored from elsewhere would, and
// cuts every watch so each resumes into a resync.
func (f *fakeWorld) newEpoch() {
	f.mu.Lock()
	f.epoch = "e2"
	f.log = nil
	streams := f.streams
	f.streams = nil
	f.mu.Unlock()
	for _, conn := range streams {
		if err := conn.Close(); err != nil {
			f.t.Errorf("cut watch: %v", err)
		}
	}
}

func (f *fakeWorld) setFailing(docPath string, failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if failing {
		f.failing[docPath] = true
	} else {
		delete(f.failing, docPath)
	}
}

// reads is how often docPath was fetched.
func (f *fakeWorld) reads(docPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches[docPath]
}

func (f *fakeWorld) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeWorld) fetch(docPath string) (fetch.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches[docPath]++
	if f.failing[docPath] {
		return fetch.Result{}, errors.New("world unavailable")
	}
	doc := f.docs[docPath]
	switch {
	case doc == nil:
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	case doc.private:
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotPermitted}}, nil
	case doc.archived:
		return fetchtest.Archived(), nil
	}
	return fetchtest.Head(doc.body, doc.version, map[string]string{"etag": fmt.Sprintf("e%d", doc.version)}), nil
}

// list answers one directory as the server would to an anonymous reader.
func (f *fakeWorld) list(dir string) fetch.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	prefix := strings.TrimSuffix(dir, "/") + "/"
	names := map[string]bool{}
	for docPath, doc := range f.docs {
		rest, ok := strings.CutPrefix(docPath, prefix)
		if !ok || doc.archived || doc.private {
			continue
		}
		if name, _, nested := strings.Cut(rest, "/"); nested {
			names[name+"/"] = true
		} else {
			names[name] = true
		}
	}
	return fetchtest.ListPage(dir, "", slices.Sorted(maps.Keys(names))...)
}

// serve streams the feed after since to conn until either end closes.
func (f *fakeWorld) serve(conn net.Conn, since protocol.Cursor) {
	defer func() {
		if err := conn.Close(); err != nil {
			f.t.Errorf("close watch: %v", err)
		}
	}()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		if _, err := io.Copy(io.Discard, conn); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			f.t.Errorf("drain watch: %v", err)
		}
	}()
	f.mu.Lock()
	next := len(f.log)
	first := protocol.WatchControl(protocol.StatusOK, f.cursorAt(next))
	if !since.IsZero() {
		if since.Epoch != f.epoch || since.Seq > uint64(len(f.log)) {
			first = protocol.WatchControl(protocol.StatusResync, f.cursorAt(next))
		} else {
			next = int(since.Seq)
			first = protocol.WatchControl(protocol.StatusOK, since)
		}
	}
	f.streams = append(f.streams, conn)
	f.mu.Unlock()
	if _, err := first.WriteTo(conn); err != nil {
		return
	}
	for {
		f.mu.Lock()
		events := slices.Clone(f.log[min(next, len(f.log)):])
		next += len(events)
		changed := f.changed
		f.mu.Unlock()
		for _, ev := range events {
			if _, err := ev.Block().WriteTo(conn); err != nil {
				return
			}
		}
		select {
		case <-changed:
		case <-closed:
			return
		}
	}
}

// fakeSource is the worlds by name, read in process.
type fakeSource map[string]*fakeWorld

func (s fakeSource) Watch(ctx context.Context, r fetch.WatchRequest) (*fetch.Watch, error) {
	world := s[r.Host]
	dial := fetch.ConnDialer(func(_ context.Context, req protocol.Request) (net.Conn, error) {
		var since protocol.Cursor
		if raw := req.Metadata["since"]; raw != "" {
			var err error
			if since, err = protocol.ParseCursor(raw); err != nil {
				return nil, err
			}
		}
		client, server := net.Pipe()
		go world.serve(server, since)
		return client, nil
	})
	return fetch.NewWatch(ctx, dial, r, 5*time.Second)
}

func (s fakeSource) Fetch(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	return s[r.Host].fetch(r.Path)
}

func (s fakeSource) List(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
	return s[r.Host].list(r.Path), nil
}

// fakeHub is a versioned store with the write rules the deriver relies on:
// expected-version checks, and an unchanged body adding no version. Each
// version is kept under the path and under its versioned path.
type fakeHub struct {
	mu      sync.Mutex
	docs    map[string]protocol.Response
	heads   map[string]int
	writes  []string        // every checkpoint path a new version was written at
	exports int             // publishes of the export, landed or not
	failing map[string]bool // writes that fail for now
}

func newFakeHub() *fakeHub {
	return &fakeHub{docs: map[string]protocol.Response{}, heads: map[string]int{}, failing: map[string]bool{}}
}

func (h *fakeHub) io() generation.IO {
	return generation.IO{Fetch: h.fetch, Publish: h.publish}
}

func (h *fakeHub) fetch(_ context.Context, docPath string) (protocol.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if resp, ok := h.docs[docPath]; ok {
		return resp, nil
	}
	return protocol.Response{Status: protocol.StatusNotFound}, nil
}

func (h *fakeHub) publish(_ context.Context, docPath, body string, expected int) (protocol.Response, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if docPath == graphstore.LegacyExportPath {
		h.exports++
	}
	if h.failing[docPath] {
		return protocol.Response{}, errors.New("hub unavailable")
	}
	head := h.heads[docPath]
	if expected >= 0 && expected != head {
		return protocol.Response{Status: protocol.StatusConflict}, nil
	}
	if head == 0 || h.docs[docPath].Body != body {
		head++
		resp := fetchtest.Head(body, head, map[string]string{"content-hash": generation.BodyHash(body)}).Response
		h.docs[docPath], h.docs[protocol.VersionPath(docPath, head)], h.heads[docPath] = resp, resp, head
		if docPath != graphstore.LegacyExportPath { // counted in exports
			h.writes = append(h.writes, docPath)
		}
	}
	return protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": fmt.Sprint(head)}}, nil
}

func (h *fakeHub) setFailing(docPath string, failing bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failing[docPath] = failing
}

// written is every checkpoint path written since the mark.
func (h *fakeHub) written(mark int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.writes[mark:])
}

// exportWrites is how often the export was published, even when the
// body was unchanged or the write conflicted.
func (h *fakeHub) exportWrites() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exports
}

func (h *fakeHub) mark() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.writes)
}
