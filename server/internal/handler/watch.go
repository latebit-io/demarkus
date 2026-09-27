package handler

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// WatchOptions is how the caller manages a WATCH stream's lifetime.
type WatchOptions struct {
	// Subscribed runs once the acknowledgement is written, so the caller can
	// release what a request holds: from then on the stream is a watch.
	Subscribed func()
	// WriteTimeout bounds each block write when the stream takes deadlines;
	// zero leaves the caller's deadlines alone.
	WriteTimeout time.Duration
}

type deadlineWriter interface {
	SetWriteDeadline(time.Time) error
}

// watchCall is one WATCH stream: the request, its scope ("/" or a prefix
// ending in "/" or one document), and once subscribed its subscription.
type watchCall struct {
	w     io.Writer
	req   protocol.Request
	scope string
	opts  WatchOptions
	sub   *changefeed.Subscription
}

func (call *watchCall) token() string { return call.req.Metadata["auth"] }

// serveWatch subscribes the stream to the change hub and pumps blocks until
// the stream, the hub or the authorization ends it (SPEC §6.8).
func (h *Handler) serveWatch(ctx context.Context, call *watchCall) {
	w, req, scope := call.w, call.req, call.scope
	if h.changes == nil {
		h.writeError(w, protocol.StatusBadRequest, "unsupported verb: "+protocol.VerbWatch)
		return
	}
	if _, ok := protocol.IsHashPath(req.Path); ok {
		h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
		return
	}
	var since protocol.Cursor
	if raw := req.Metadata["since"]; raw != "" {
		var err error
		if since, err = protocol.ParseCursor(raw); err != nil {
			h.writeError(w, protocol.StatusBadRequest, "invalid since cursor")
			return
		}
	}
	if err := h.tokenStore().AuthorizeRead(call.token(), scope); err != nil {
		h.writeAuthDenied(w, req, err)
		return
	}
	sub, err := h.changes.Subscribe(scope, since)
	if err != nil {
		// Subscribe refuses only a cursor it cannot resume from.
		h.logger.Info("watch resync", "path", sanitize(scope), "since", since.String(), "error", err)
		h.writeBlock(call, protocol.WatchControl(protocol.StatusResync, h.changes.Head()))
		return
	}
	call.sub = sub
	if !h.writeBlock(call, protocol.WatchControl(protocol.StatusOK, sub.Cursor())) {
		return
	}
	if call.opts.Subscribed != nil {
		call.opts.Subscribed()
	}
	h.logger.Info("watch", "path", sanitize(scope), "resumed", !since.IsZero())
	h.pumpWatch(ctx, call)
}

// pumpWatch writes events, heartbeats and the terminal block. Every event is
// checked against the current token store and omitted when unreadable; a
// heartbeat rechecks the scope, so a revoked token ends an idle watch too.
func (h *Handler) pumpWatch(ctx context.Context, call *watchCall) {
	sub, scope, token := call.sub, call.scope, call.token()
	// The heartbeat is due a fixed time after the last block written, not
	// after the last event seen: omitted events must not silence it.
	nextBeat := time.Now().Add(h.heartbeat)
	for {
		waitCtx, cancel := context.WithDeadline(ctx, nextBeat)
		ev, err := sub.Next(waitCtx)
		cancel()
		switch {
		case err == nil:
			if h.tokenStore().AuthorizeRead(token, ev.Path) != nil {
				continue
			}
			block, ok := h.eventBlock(scope, ev)
			if !ok {
				continue
			}
			if !h.writeBlock(call, block) {
				return
			}
			nextBeat = time.Now().Add(h.heartbeat)
		case ctx.Err() != nil:
			h.logger.Debug("watch ended", "path", sanitize(scope), "error", ctx.Err())
			return
		case errors.Is(err, context.DeadlineExceeded):
			if authErr := h.tokenStore().AuthorizeRead(token, scope); authErr != nil {
				status := protocol.StatusNotPermitted
				if auth.IsUnauthenticated(authErr) {
					status = protocol.StatusUnauthorized
				}
				h.logger.Info("watch ended by token reload", "path", sanitize(scope), "status", status)
				h.writeBlock(call, protocol.WatchControl(status, sub.Cursor()))
				return
			}
			if !h.writeBlock(call, protocol.WatchControl(protocol.StatusOK, sub.Cursor())) {
				return
			}
			nextBeat = time.Now().Add(h.heartbeat)
		case errors.Is(err, changefeed.ErrResync):
			h.logger.Info("watch resync", "path", sanitize(scope), "error", err)
			h.writeBlock(call, protocol.WatchControl(protocol.StatusResync, h.changes.Head()))
			return
		case errors.Is(err, changefeed.ErrClosed):
			h.writeBlock(call, protocol.WatchControl(protocol.StatusClosing, sub.Cursor()))
			return
		default:
			h.logger.Error("watch failed", "path", sanitize(scope), "error", err)
			return
		}
	}
}

// eventBlock encodes ev, dropping the publisher-supplied agent when it
// alone breaks the block limit; false skips an event that still cannot be
// encoded, since replaying it would only end the stream again.
func (h *Handler) eventBlock(scope string, ev changefeed.Event) (protocol.WatchBlock, bool) {
	event := protocol.WatchEvent{
		Cursor:  protocol.Cursor{Epoch: h.changes.Epoch(), Seq: ev.Seq},
		Path:    ev.Path,
		Version: ev.Version,
		Hash:    ev.Hash,
		Op:      ev.Op,
		Agent:   ev.Agent,
	}
	if _, err := event.Block().WriteTo(io.Discard); err == nil {
		return event.Block(), true
	}
	event.Agent = ""
	if _, err := event.Block().WriteTo(io.Discard); err != nil {
		h.logger.Warn("watch event skipped", "path", sanitize(scope), "event", sanitize(ev.Path), "error", err)
		return protocol.WatchBlock{}, false
	}
	return event.Block(), true
}

// writeBlock writes one block under the write timeout; false means the peer
// is gone and the watch is over.
func (h *Handler) writeBlock(call *watchCall, block protocol.WatchBlock) bool {
	if dw, ok := call.w.(deadlineWriter); ok && call.opts.WriteTimeout > 0 {
		if err := dw.SetWriteDeadline(time.Now().Add(call.opts.WriteTimeout)); err != nil {
			h.logger.Debug("setting watch write deadline", "error", err)
		}
	}
	if _, err := block.WriteTo(call.w); err != nil {
		h.logger.Debug("watch write failed", "error", err)
		return false
	}
	return true
}
