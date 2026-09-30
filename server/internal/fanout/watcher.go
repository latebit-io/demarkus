package fanout

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
)

const (
	// maxBatchBytes bounds one stream write; a watcher further behind
	// writes again at once.
	maxBatchBytes = 64 << 10
	// keepBufBytes and keepPending bound what a watcher keeps between
	// writes, so one burst does not pin a burst-sized buffer per stream.
	keepBufBytes = 16 << 10
	keepPending  = 64
)

// Request is one WATCH after the handler validated it. Scope is "/", a
// prefix ending in "/" or one document path; Token may be empty.
type Request struct {
	Scope string
	Token string
	Since protocol.Cursor
	// Coalesce delivers only the newest event per path of those pending
	// for a lagging watcher, in order.
	Coalesce bool
}

// LimitError is Serve's refusal at the world or the connection cap.
type LimitError struct{ Limit string }

func (e *LimitError) Error() string { return "watch limit reached: " + e.Limit }

// end is why a pump returned: the terminal status to write with its cursor,
// or an empty status when the peer is gone and nothing is left to say.
type end struct {
	status string
	cursor protocol.Cursor
}

// watcher is one attached stream: a read index into the ring, and until it
// caught up with the ring, a hub subscription for what came before.
type watcher struct {
	fanout   *Fanout
	group    *group
	conn     *quicserve.ConnState
	coalesce bool
	ack      protocol.Cursor
	backlog  *changefeed.Subscription
	index    uint64
	tick     uint64
	wrote    bool
	pending  []entry
	buf      []byte
	seen     map[string]struct{}
}

// Serve writes the whole watch to out (ack, events, heartbeats, terminal
// block) until ctx ends or the watch is over. A refusal before the ack is
// returned: an auth verdict or a *LimitError; a bad since is a resync block.
func (f *Fanout) Serve(ctx context.Context, req Request, out io.Writer) error {
	w, err := f.attach(ctx, req)
	if errors.Is(err, changefeed.ErrResync) {
		f.logger.Info("watch resync", "scope", req.Scope, "since", req.Since.String(), "error", err)
		_, writeErr := protocol.WatchControl(protocol.StatusResync, f.hub.Head()).WriteTo(out)
		return writeErr
	}
	if err != nil {
		return err
	}
	defer w.detach()
	if _, err := protocol.WatchControl(protocol.StatusOK, w.ack).WriteTo(out); err != nil {
		return nil //nolint:nilerr // the peer is gone; nothing is left to say
	}
	f.logger.Info("watch", "scope", req.Scope, "resumed", !req.Since.IsZero())
	if e := w.pump(ctx, out); e.status != "" {
		f.logger.Info("watch ended", "scope", req.Scope, "status", e.status)
		if _, err := protocol.WatchControl(e.status, e.cursor).WriteTo(out); err != nil {
			f.logger.Debug("watch terminal write failed", "error", err)
		}
	}
	return nil
}

// attach admits a watch: the token must read the scope now, a slot must be
// free at the world and the connection, and since must be resumable.
func (f *Fanout) attach(ctx context.Context, req Request) (*watcher, error) {
	token := auth.HashToken(req.Token)
	if err := f.tokenStore().AuthorizeReadHashed(token, req.Scope); err != nil {
		return nil, err
	}
	conn := quicserve.ConnStateFromContext(ctx)
	f.mu.Lock()
	if f.watchers >= f.maxWatches {
		f.mu.Unlock()
		return nil, &LimitError{Limit: "world"}
	}
	if conn != nil && f.maxPerConn > 0 && int(conn.Watches.Add(1)) > f.maxPerConn {
		conn.Watches.Add(-1)
		f.mu.Unlock()
		return nil, &LimitError{Limit: "connection"}
	}
	r := f.run
	if r == nil {
		var err error
		if r, err = f.startLocked(); err != nil {
			f.mu.Unlock()
			f.releaseConn(conn)
			return nil, err
		}
	}
	w := &watcher{fanout: f, conn: conn, coalesce: req.Coalesce, tick: r.tick, group: f.joinLocked(groupKey{scope: req.Scope, token: token}, r.next)}
	f.watchers++
	// The ack is the head for a new watch, the client's since for a resume.
	w.index, w.ack = r.next, req.Since
	inRing := !req.Since.IsZero() && req.Since.Epoch == f.hub.Epoch() && req.Since.Seq+1 >= max(r.oldest(), w.group.from) && req.Since.Seq < r.next
	if inRing {
		w.index = req.Since.Seq + 1
	}
	if req.Since.IsZero() {
		w.ack = f.cursor(r.next - 1)
	}
	f.mu.Unlock()
	if req.Since.IsZero() || inRing {
		return w, nil
	}
	// Older than the ring: the hub serves up to the ring's next seq, from
	// its own ring or the store's backlog.
	sub, err := f.hub.Subscribe(ctx, req.Scope, req.Since)
	if err != nil {
		w.detach()
		return nil, err
	}
	w.backlog = sub
	return w, nil
}

func (f *Fanout) releaseConn(conn *quicserve.ConnState) {
	if conn != nil && f.maxPerConn > 0 {
		conn.Watches.Add(-1)
	}
}

// detach releases the slots and the group; the last watcher out stops the
// reader and the sweep and frees the ring.
func (w *watcher) detach() {
	f := w.fanout
	f.mu.Lock()
	f.leaveLocked(w.group)
	f.watchers--
	var stopped *run
	if f.watchers == 0 && f.run != nil {
		stopped = f.stopLocked()
	}
	f.mu.Unlock()
	f.releaseConn(w.conn)
	if stopped != nil {
		stopped.done.Wait()
	}
}

// pump writes events and heartbeats to out until the watch ends, and
// returns the terminal block to write. A write that fails, or ctx ending
// without an auth verdict as its cause, means the peer is gone: an empty end.
func (w *watcher) pump(ctx context.Context, out io.Writer) end {
	if w.backlog != nil {
		if e := w.catchUp(); e.status != "" {
			return e
		}
	}
	for {
		e, wait := w.gather()
		switch {
		case e.status != "":
			return e
		case len(w.pending) > 0:
			if err := w.flush(out); err != nil {
				return end{}
			}
			continue
		case wait == nil:
			if err := w.heartbeat(out); err != nil {
				return end{}
			}
			continue
		}
		select {
		case <-wait:
		case <-ctx.Done():
			// A credential that lapses mid-watch ends it with its verdict.
			if cause := context.Cause(ctx); auth.IsDenial(cause) {
				return end{status: auth.DenialStatus(cause), cursor: w.cursor()}
			}
			return end{}
		}
	}
}

// gather copies the pending entries of the watcher's group out of the ring
// and reports the terminal end, or the channel to wait on; a nil channel
// with nothing pending means a heartbeat is due.
func (w *watcher) gather() (terminal end, wait <-chan struct{}) {
	f := w.fanout
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.run
	switch {
	case w.group.denied != nil:
		return end{status: auth.DenialStatus(w.group.denied), cursor: w.cursor()}, nil
	case r.closed:
		return end{status: protocol.StatusClosing, cursor: w.cursor()}, nil
	case w.index < r.oldest():
		return end{status: protocol.StatusResync, cursor: f.hub.Head()}, nil
	}
	size := 0
	for w.index < r.next && size < maxBatchBytes {
		e := &r.buf[r.slot(w.index)]
		w.index++
		// A stale slot is a seq the reader did not append this run.
		if e.seq == w.index-1 && e.block != nil && slices.Contains(e.groups, w.group.id) {
			w.pending = append(w.pending, *e)
			size += len(e.block)
		}
	}
	if len(w.pending) > 0 {
		return end{}, nil
	}
	if r.tick != w.tick {
		idle := !w.wrote
		w.tick, w.wrote = r.tick, false
		if idle {
			return end{}, nil
		}
	}
	return end{}, w.group.notify
}

// flush writes every pending block in one write, the newest per path only
// when coalescing, and drops the references it held.
func (w *watcher) flush(out io.Writer) error {
	if w.coalesce && len(w.pending) > 1 {
		w.coalescePending()
	}
	w.buf = w.buf[:0]
	for i := range w.pending {
		w.buf = append(w.buf, w.pending[i].block...)
	}
	clear(w.pending)
	w.pending = w.pending[:0]
	w.wrote = true
	_, err := out.Write(w.buf)
	if cap(w.buf) > keepBufBytes {
		w.buf = nil
	}
	if cap(w.pending) > keepPending {
		w.pending, w.seen = nil, nil
	}
	return err
}

// coalescePending keeps, in order, the last pending entry of every path.
func (w *watcher) coalescePending() {
	if w.seen == nil {
		w.seen = map[string]struct{}{}
	}
	clear(w.seen)
	for i := len(w.pending) - 1; i >= 0; i-- {
		path := w.pending[i].path
		if _, dup := w.seen[path]; dup {
			w.pending[i].block = nil
			continue
		}
		w.seen[path] = struct{}{}
	}
	w.pending = slices.DeleteFunc(w.pending, func(e entry) bool { return e.block == nil })
}

func (w *watcher) heartbeat(out io.Writer) error {
	w.wrote = true
	_, err := protocol.WatchControl(protocol.StatusOK, w.cursor()).WriteTo(out)
	return err
}

// cursor is the resume position: just before the next index.
func (w *watcher) cursor() protocol.Cursor { return w.fanout.cursor(w.index - 1) }

// catchUp moves what the hub holds before the ring's start into pending,
// checked and encoded for this watcher alone, so the first flush coalesces
// it with the ring's own. Only a resume older than the ring pays for it.
func (w *watcher) catchUp() end {
	f := w.fanout
	sub := w.backlog
	w.backlog = nil
	store := f.tokenStore()
	for {
		ev, err := sub.TryNext()
		switch {
		case errors.Is(err, changefeed.ErrIdle) || err == nil && ev.Seq >= w.index:
			return end{}
		case errors.Is(err, changefeed.ErrResync):
			return end{status: protocol.StatusResync, cursor: f.hub.Head()}
		case errors.Is(err, changefeed.ErrClosed):
			return end{status: protocol.StatusClosing, cursor: w.ack}
		case err != nil:
			f.logger.Error("watch catch-up failed", "error", err)
			return end{}
		}
		if store.AuthorizeReadHashed(w.group.key.token, ev.Path) != nil {
			continue
		}
		if block := f.encode(ev); block != nil {
			w.pending = append(w.pending, entry{seq: ev.Seq, path: ev.Path, block: block})
		}
	}
}
