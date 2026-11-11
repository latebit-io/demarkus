package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/neardup"
	"github.com/latebit-io/demarkus/protocol/render"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// LOOKUP result bounds.
const (
	// defaultLookupLimit is the result count used when a request omits limit.
	defaultLookupLimit = 10
	// maxLookupResults is the hard cap on LOOKUP results regardless of limit.
	maxLookupResults = 1000
	// maxLookupTerms bounds distinct query terms: each one costs a scored
	// pass per document, and 64KB of frontmatter would otherwise buy tens
	// of thousands.
	maxLookupTerms = 32
)

// lookupRows converts catalog results into the rows protocol/render draws.
func lookupRows(results []catalog.Result) []render.LookupRow {
	rows := make([]render.LookupRow, 0, len(results))
	for i := range results {
		r := &results[i]
		snippet := r.Snippet
		if r.Hit.Verdict != neardup.None {
			snippet = r.Hit.Why()
		}
		rows = append(rows, render.LookupRow{
			Path:       r.Path,
			Anchor:     r.Anchor,
			Importance: r.Importance,
			Title:      r.DisplayTitle(),
			Tags:       r.Tags,
			Snippet:    snippet,
		})
	}
	return rows
}

// lookupSpec is a LOOKUP request's validated fields.
type lookupSpec struct {
	query        string
	mode         catalog.Mode
	matchCarried bool
	limit        int
	preds        []catalog.Predicate
}

// parseLookup validates the request's keys.
func parseLookup(req protocol.Request) (lookupSpec, error) {
	var spec lookupSpec
	matchValue, matchCarried := req.Metadata["match"]
	mode, err := catalog.ParseMode(matchValue)
	if err != nil {
		return spec, err
	}
	query := strings.TrimSpace(req.Metadata["query"])
	switch {
	case mode == catalog.MatchSimilar:
		// The draft is the body; query only names the report (SPEC 6.7).
		if strings.TrimSpace(req.Body) == "" {
			return spec, errors.New("match similar needs the draft as the request body")
		}
	// "*" is the match-all query (whole catalog under scope, importance
	// order); anything else needs at least 2 characters of subject.
	case query != "*" && len([]rune(query)) < 2:
		return spec, errors.New("query must be at least 2 characters")
	}
	// Bounded here, not per backend, so both reject the same queries.
	if n := catalog.CountTerms(query, maxLookupTerms); n > maxLookupTerms {
		return spec, fmt.Errorf("query has too many terms: %d > %d", n, maxLookupTerms)
	}
	limit := defaultLookupLimit
	if lv := req.Metadata["limit"]; lv != "" {
		n, err := strconv.Atoi(lv)
		if err != nil || n < 1 {
			return spec, errors.New("invalid limit")
		}
		limit = n
	}
	preds, err := catalog.ParseFilter(req.Metadata["filter"])
	if err != nil {
		return spec, err
	}
	return lookupSpec{query: query, mode: mode, matchCarried: matchCarried, limit: min(limit, maxLookupResults), preds: preds}, nil
}

func (h *Handler) handleLookup(ctx context.Context, call *readCall) {
	w, req, reader := call.w, call.req, call.view
	lookup := storagebackend.CatalogReader(call.view)

	spec, err := parseLookup(req)
	if err != nil {
		h.writeError(w, protocol.StatusBadRequest, err.Error())
		return
	}
	mode := spec.mode

	// Scope must be the whole-server root or an existing directory. A scope
	// the requester may not read is answered as if it existed, rows filtered:
	// not-found there would tell a protected directory from a missing one.
	token := req.Metadata["auth"]
	if req.Path != "/" && h.checkReadAuth(req.Path, token) == nil {
		isDir, derr := reader.IsDir(ctx, req.Path)
		if derr != nil && !errors.Is(derr, storagebackend.ErrNotFound) {
			h.logger.Error("lookup isdir check failed", "path", sanitize(req.Path), "error", derr)
			h.writeError(w, protocol.StatusServerError, "internal error")
			return
		}
		if !isDir {
			h.logger.Info("lookup scope not found", "path", sanitize(req.Path))
			h.writeError(w, protocol.StatusNotFound, req.Path+" not found")
			return
		}
	}

	// Unreadable documents are skipped before the cap, so they never displace
	// authorized matches nor reveal their existence (no path, not counted).
	opts := catalog.Options{Scope: req.Path, Filter: spec.preds, Max: spec.limit, Match: mode, Skip: h.unreadable(token), Prior: h.lookupPrior}
	if mode == catalog.MatchSimilar {
		draft := neardup.AnalyzeWhole(req.Body)
		opts.Draft = &draft
	}
	rows, err := lookup.Lookup(ctx, spec.query, opts)
	if errors.Is(err, storagebackend.ErrBodyMatchUnavailable) {
		// The view's snapshot has no section index yet: answer from the
		// catalog on the same snapshot and echo the mode answered (SPEC 6.7).
		// A similar request has no catalog answer; its table is empty.
		rows, err = nil, nil
		if mode != catalog.MatchSimilar {
			opts.Match = catalog.MatchCatalog
			rows, err = lookup.Lookup(ctx, spec.query, opts)
		}
		mode = catalog.MatchCatalog
	}
	if err != nil {
		h.logger.Error("lookup failed", "path", sanitize(req.Path), "error", err)
		h.writeError(w, protocol.StatusServerError, "internal error")
		return
	}

	// Echo match only when asked, so a request without it gets the
	// response it always got, byte for byte.
	echo := ""
	if spec.matchCarried {
		echo = string(mode)
	}
	h.writeResponse(w, render.LookupResponse(spec.query, req.Path, lookupRows(rows), echo))
}
