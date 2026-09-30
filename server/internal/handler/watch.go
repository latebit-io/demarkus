package handler

import (
	"context"
	"errors"
	"io"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/fanout"
)

// serveWatch attaches the stream to the world's fan-out and pumps blocks
// until the stream, the hub or the authorization ends it (SPEC §6.8).
func (h *Handler) serveWatch(ctx context.Context, w io.Writer, req protocol.Request) {
	if h.watches == nil {
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
	coalesce := req.Metadata["coalesce"]
	if coalesce != "" && coalesce != "path" {
		h.writeError(w, protocol.StatusBadRequest, "unsupported coalesce: "+coalesce)
		return
	}
	scope := scopePath(req.Path)
	err := h.watches.Serve(ctx, fanout.Request{Scope: scope, Token: req.Metadata["auth"], Since: since, Coalesce: coalesce != ""}, w)
	var limited *fanout.LimitError
	switch {
	case errors.As(err, &limited):
		h.logger.Warn("watch limit reached", "limit", limited.Limit)
		h.writeError(w, protocol.StatusRateLimited, err.Error())
	case auth.IsDenial(err):
		h.writeAuthDenied(w, req, err)
	case err != nil:
		h.logger.Error("watch failed", "path", sanitize(scope), "error", err)
		h.writeError(w, protocol.StatusServerError, "watch failed")
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
