package handler

import (
	"context"
	"io"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

// authorizeWrite checks publish (govern for a restricted governance path, the
// graph capability for generated graph data under a grant) and answers the
// denial itself: a grant on ctx decides alone, else the token; neither, no write.
func (h *Handler) authorizeWrite(ctx context.Context, w io.Writer, req protocol.Request) (tokenLabel string, ok bool) {
	gov := storefmt.IsGovernancePath(req.Path)
	restricted := gov && h.restrictGov
	if grant, granted := protocol.GrantFrom(ctx); granted {
		switch {
		case !auth.Permits(&grant, req.Path):
			h.Deny(w, req, auth.ErrNotPermitted)
		case restricted && !grant.Govern:
			h.denyGovernance(w, req)
		case !grant.Graph && storefmt.GeneratedGraphTarget(req.Path):
			h.denyGraph(w, req)
		default:
			return grant.Label, true
		}
		return "", false
	}
	ts := h.tokenStore()
	if ts == nil {
		h.writeError(w, protocol.StatusNotPermitted, writeActivity[req.Verb]+" requires auth configuration")
		return "", false
	}
	var ops []string
	switch {
	case restricted:
		ops = []string{protocol.OpGovern}
	case gov: // an open world takes govern too, so governors exist before it restricts
		ops = []string{"publish", protocol.OpGovern}
	default:
		ops = []string{"publish"}
	}
	tokenLabel, err := ts.AuthorizeAny(req.Metadata["auth"], req.Path, ops...)
	switch {
	case err == nil:
		return tokenLabel, true
	case restricted && auth.DenialStatus(err) == protocol.StatusNotPermitted:
		h.denyGovernance(w, req)
	default:
		h.Deny(w, req, err)
	}
	return "", false
}

// denyGovernance names what a governance write lacks, so an operator knows
// which credential to reach for.
func (h *Handler) denyGovernance(w io.Writer, req protocol.Request) {
	h.logger.Warn("governance write refused", "audit", true, "operation", req.Verb, "path", sanitize(req.Path))
	h.writeError(w, protocol.StatusNotPermitted, "governance document: "+writeActivity[req.Verb]+" "+protocol.GovernancePrefix+" requires the "+protocol.OpGovern+" operation")
}

// denyGraph refuses a grant without Graph: only the deriver's writes
// generated graph data.
func (h *Handler) denyGraph(w io.Writer, req protocol.Request) {
	h.logger.Warn("graph data write refused", "audit", true, "operation", req.Verb, "path", sanitize(req.Path))
	h.writeError(w, protocol.StatusNotPermitted, "generated graph data: derived from the documents, never written directly")
}

// authorizeRead reports whether a read may proceed: a path no read token
// covers is public. On a missing or invalid token it writes the denial and
// returns false.
func (h *Handler) authorizeRead(w io.Writer, req protocol.Request) bool {
	if err := h.checkReadAuth(req.Path, req.Metadata["auth"]); err != nil {
		h.Deny(w, req, err)
		return false
	}
	return true
}

// Deny answers a failed authorization, err being an auth package sentinel;
// a caller that refused req before Serve answers through it too.
func (h *Handler) Deny(w io.Writer, req protocol.Request, err error) {
	status := auth.DenialStatus(err)
	h.logger.Warn(status, "operation", req.Verb, "path", sanitize(req.Path))
	if status == protocol.StatusUnauthorized {
		h.writeError(w, status, "authentication required")
		return
	}
	h.writeError(w, status, "insufficient permissions")
}

// checkReadAuth decides a read without writing a response: nil when allowed,
// otherwise the auth verdict.
func (h *Handler) checkReadAuth(reqPath, token string) error {
	return h.tokenStore().AuthorizeRead(token, storefmt.ScopePath(reqPath))
}

// unreadable is LOOKUP's skip for the requester's token, hashed once: true
// for a canonical document path the token may not read.
func (h *Handler) unreadable(token string) func(string) bool {
	tokens, hashed := h.tokenStore(), auth.HashToken(token)
	return func(p string) bool { return tokens.AuthorizeReadHashed(hashed, p) != nil }
}

// readableBy is the read check a write's notes about other documents pass,
// the one LOOKUP filters its rows by.
func (h *Handler) readableBy(req protocol.Request) func(string) bool {
	return func(p string) bool { return h.checkReadAuth(p, req.Metadata["auth"]) == nil }
}
