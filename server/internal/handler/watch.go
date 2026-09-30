package handler

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
)

// serveWatch subscribes the stream to the change hub and pumps blocks until
// the stream, the hub or the authorization ends it (SPEC §6.8).
func (h *Handler) serveWatch(ctx context.Context, w io.Writer, req protocol.Request) {
	if h.changes == nil {
		h.writeError(w, protocol.StatusBadRequest, "unsupported verb: "+protocol.VerbWatch)
		return
	}
	if h.refuseHashPath(w, req.Path) {
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
	token := req.Metadata["auth"]
	if err := h.checkReadAuth(req.Path, token); err != nil {
		h.writeAuthDenied(w, req, err)
		return
	}
	scope := scopePath(req.Path)
	sub, err := h.changes.Subscribe(ctx, scope, since)
	if err != nil {
		// Subscribe refuses only a cursor it cannot resume from.
		h.logger.Info("watch resync", "path", sanitize(scope), "since", since.String(), "error", err)
		h.writeBlock(w, protocol.WatchControl(protocol.StatusResync, h.changes.Head()))
		return
	}
	if !h.writeBlock(w, protocol.WatchControl(protocol.StatusOK, sub.Cursor())) {
		return
	}
	h.logger.Info("watch", "path", sanitize(scope), "resumed", !since.IsZero())
	if status, cursor := h.pumpWatch(ctx, w, token, sub); status != "" {
		h.writeBlock(w, protocol.WatchControl(status, cursor))
	}
}

// refuseHashPath answers a write or watch on a content address, which no
// verb but FETCH accepts.
func (h *Handler) refuseHashPath(w io.Writer, path string) bool {
	if _, ok := protocol.IsHashPath(path); !ok {
		return false
	}
	h.writeError(w, protocol.StatusBadRequest, "paths matching /sha256-<hash> are reserved")
	return true
}

// pumpWatch writes events and heartbeats until the watch ends, and returns
// the terminal block to write, or an empty status when there is nothing
// left to say.
func (h *Handler) pumpWatch(ctx context.Context, w io.Writer, token string, sub *changefeed.Subscription) (string, protocol.Cursor) {
	for {
		if status, cursor, ended := h.pumpUntilBeat(ctx, w, token, sub); ended {
			return status, cursor
		}
	}
}

// pumpUntilBeat serves until one block is written or the watch ends. The
// heartbeat is due a fixed time after the last block written, so events the
// current token store refuses (omitted) cannot silence it or its recheck.
func (h *Handler) pumpUntilBeat(ctx context.Context, w io.Writer, token string, sub *changefeed.Subscription) (status string, cursor protocol.Cursor, ended bool) {
	scope := sub.Scope()
	wait, cancel := context.WithDeadline(ctx, time.Now().Add(h.heartbeat))
	defer cancel()
	for {
		ev, err := sub.Next(wait)
		switch {
		case err == nil:
			if h.tokenStore().AuthorizeRead(token, ev.Path) != nil {
				continue
			}
			return "", protocol.Cursor{}, !h.writeBlock(w, ev.Block(h.changes.Epoch()))
		case ctx.Err() != nil:
			h.logger.Debug("watch ended", "path", sanitize(scope), "error", ctx.Err())
			return "", protocol.Cursor{}, true
		case errors.Is(err, context.DeadlineExceeded):
			if authErr := h.tokenStore().AuthorizeRead(token, scope); authErr != nil {
				h.logger.Info("watch ended by token reload", "path", sanitize(scope))
				return authStatus(authErr), sub.Cursor(), true
			}
			return "", protocol.Cursor{}, !h.writeBlock(w, protocol.WatchControl(protocol.StatusOK, sub.Cursor()))
		case errors.Is(err, changefeed.ErrResync):
			h.logger.Info("watch resync", "path", sanitize(scope), "error", err)
			return protocol.StatusResync, h.changes.Head(), true
		case errors.Is(err, changefeed.ErrClosed):
			return protocol.StatusClosing, sub.Cursor(), true
		default:
			h.logger.Error("watch failed", "path", sanitize(scope), "error", err)
			return "", protocol.Cursor{}, true
		}
	}
}

// writeBlock writes one block; false means the peer is gone and the watch
// is over. A block the codec refuses is skipped instead: nothing reached
// the stream, and ending it would only replay the same event.
func (h *Handler) writeBlock(w io.Writer, block protocol.WatchBlock) bool {
	_, err := block.WriteTo(w)
	switch {
	case err == nil:
		return true
	case errors.Is(err, protocol.ErrMalformedWatchBlock):
		h.logger.Warn("watch block skipped", "block", sanitize(block.Metadata["path"]), "error", err)
		return true
	default:
		h.logger.Debug("watch write failed", "error", err)
		return false
	}
}
