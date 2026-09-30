package fetch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// Every request names its server as Host (host:port for a direct client, a
// world name behind the broker) and may carry a capability Token. An empty
// Token sends no auth.

// FetchRequest reads one document.
type FetchRequest struct { //nolint:revive // named for the FETCH verb; fetch.Request would not say which of seven
	Host, Path, Token string
	// IfNoneMatch makes the read conditional for callers that track freshness
	// themselves; a match answers not-modified with an empty body.
	IfNoneMatch string
}

// ListRequest reads one page of a directory listing.
type ListRequest struct {
	Host, Path, Token string
	// IncludeArchived also lists archived documents and the directories that
	// hold nothing else.
	IncludeArchived bool
	// Cursor continues the same directory and archive mode listing.
	Cursor string
	// PageSize caps the entries returned; zero leaves it to the server.
	PageSize int
}

// VersionsRequest reads a document's version history.
type VersionsRequest struct {
	Host, Path, Token string
}

// LookupRequest queries the catalog under Scope. Query is required; zero
// values of the rest are omitted so the server applies its own defaults.
type LookupRequest struct {
	Host, Scope, Token string
	Query              string
	Filter             string // comma separated key=value predicates
	Limit              int    // max results; <= 0 lets the server choose
	Match              string // MatchCatalog (default) or MatchBody
}

// WriteRequest is a PUBLISH or an APPEND. ExpectedVersion is the optimistic
// lock: 0 creates only, above 0 must equal the current version, below 0 skips
// the check (PUBLISH only). The zero value therefore never overwrites.
type WriteRequest struct {
	Host, Path, Token string
	Body              string
	ExpectedVersion   int
	Metadata          map[string]string // publisher metadata; not modified
}

// ArchiveRequest archives one document.
type ArchiveRequest struct {
	Host, Path, Token string
}

// Request is the FETCH this read encodes.
func (r FetchRequest) Request() (protocol.Request, error) {
	var extra map[string]string
	if r.IfNoneMatch != "" {
		extra = map[string]string{"if-none-match": r.IfNoneMatch}
	}
	return newRequest(protocol.VerbFetch, r.Path, r.Token, extra), nil
}

// Request is the LIST this read encodes; it refuses a page size the
// protocol does not allow.
func (r ListRequest) Request() (protocol.Request, error) {
	if r.PageSize < 0 || r.PageSize > protocol.MaxListPageSize {
		return protocol.Request{}, fmt.Errorf("LIST page size must be between 1 and %d, or 0 for the server default", protocol.MaxListPageSize)
	}
	extra := make(map[string]string)
	if r.IncludeArchived {
		extra["include-archived"] = "true"
	}
	if r.Cursor != "" {
		extra["cursor"] = r.Cursor
	}
	if r.PageSize > 0 {
		extra["page-size"] = strconv.Itoa(r.PageSize)
	}
	return newRequest(protocol.VerbList, r.Path, r.Token, extra), nil
}

// Request is the VERSIONS this read encodes.
func (r VersionsRequest) Request() (protocol.Request, error) {
	return newRequest(protocol.VerbVersions, r.Path, r.Token, nil), nil
}

// Request is the LOOKUP this query encodes; an empty query or an unknown
// match mode is refused here once for every surface, not relayed back as a
// bad-request body.
func (r LookupRequest) Request() (protocol.Request, error) {
	if r.Query == "" {
		return protocol.Request{}, errors.New("LOOKUP requires a non-empty query")
	}
	if _, err := protocol.ParseMatch(r.Match); err != nil {
		return protocol.Request{}, err
	}
	req := newRequest(protocol.VerbLookup, r.Scope, r.Token, map[string]string{"query": r.Query})
	if r.Filter != "" {
		req.Metadata["filter"] = r.Filter
	}
	if r.Limit > 0 {
		req.Metadata["limit"] = strconv.Itoa(r.Limit)
	}
	if r.Match != "" {
		req.Metadata["match"] = r.Match
	}
	return req, nil
}

// PublishRequest is the PUBLISH this write encodes.
func (r WriteRequest) PublishRequest() (protocol.Request, error) {
	req := newRequest(protocol.VerbPublish, r.Path, r.Token, r.Metadata)
	req.Body = r.Body
	if r.ExpectedVersion >= 0 {
		req.Metadata["expected-version"] = strconv.Itoa(r.ExpectedVersion)
	}
	return req, nil
}

// AppendRequest is the APPEND this write encodes; it needs a body and an
// expected version of at least 1.
func (r WriteRequest) AppendRequest() (protocol.Request, error) {
	if r.ExpectedVersion < 1 {
		return protocol.Request{}, fmt.Errorf("APPEND requires expected-version >= 1, got %d", r.ExpectedVersion)
	}
	if r.Body == "" {
		return protocol.Request{}, errors.New("APPEND requires a non-empty body")
	}
	req := newRequest(protocol.VerbAppend, r.Path, r.Token, r.Metadata)
	req.Body = r.Body
	req.Metadata["expected-version"] = strconv.Itoa(r.ExpectedVersion)
	return req, nil
}

// Request is the ARCHIVE this encodes.
func (r ArchiveRequest) Request() (protocol.Request, error) {
	return newRequest(protocol.VerbArchive, r.Path, r.Token, nil), nil
}

// Fetch retrieves a document. An unconditional, tokenless read is served
// through the response cache when one is configured.
func (c *Client) Fetch(ctx context.Context, r FetchRequest) (Result, error) {
	req, err := r.Request()
	if err != nil {
		return Result{}, err
	}
	return c.cachedRead(ctx, r.Host, req)
}

// List retrieves one page of a directory listing.
func (c *Client) List(ctx context.Context, r ListRequest) (Result, error) {
	req, err := r.Request()
	if err != nil {
		return Result{}, err
	}
	return c.cachedRead(ctx, r.Host, req)
}

// Versions retrieves the version history of a document.
func (c *Client) Versions(ctx context.Context, r VersionsRequest) (Result, error) {
	req, err := r.Request()
	if err != nil {
		return Result{}, err
	}
	return c.read(ctx, r.Host, req)
}

// Lookup queries a server's catalog and returns an importance ranked table.
func (c *Client) Lookup(ctx context.Context, r LookupRequest) (Result, error) {
	req, err := r.Request()
	if err != nil {
		return Result{}, err
	}
	return c.read(ctx, r.Host, req)
}

// Publish creates or replaces a document. It is sent at most once: a failure
// after the first byte answers protocol.ErrOutcomeUnknown.
func (c *Client) Publish(ctx context.Context, r WriteRequest) (Result, error) {
	req, err := r.PublishRequest()
	if err != nil {
		return Result{}, err
	}
	return c.write(ctx, r.Host, req)
}

// Append adds Body to the end of an existing document, so ExpectedVersion
// must be at least 1. Sent at most once, like Publish.
func (c *Client) Append(ctx context.Context, r WriteRequest) (Result, error) {
	req, err := r.AppendRequest()
	if err != nil {
		return Result{}, err
	}
	return c.write(ctx, r.Host, req)
}

// Archive marks a document as archived. Sent at most once, like Publish.
func (c *Client) Archive(ctx context.Context, r ArchiveRequest) (Result, error) {
	req, err := r.Request()
	if err != nil {
		return Result{}, err
	}
	return c.write(ctx, r.Host, req)
}

// newRequest copies meta, so the caller's map is never modified, then adds auth.
func newRequest(verb, path, token string, meta map[string]string) protocol.Request {
	req := protocol.Request{Verb: verb, Path: path, Metadata: make(map[string]string, len(meta)+2)}
	maps.Copy(req.Metadata, meta)
	if token != "" {
		req.Metadata["auth"] = token
	}
	return req
}

func (c *Client) write(ctx context.Context, host string, req protocol.Request) (Result, error) {
	return c.doWriteContext(ctx, host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConnContext(ctx, conn, req)
	})
}

// read sends an uncached read with retries.
func (c *Client) read(ctx context.Context, host string, req protocol.Request) (Result, error) {
	return c.doWithRetryContext(ctx, host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConnContext(ctx, conn, req)
	})
}

// AnsweredFromCatalog reports a body match request that the server answered
// in catalog mode: an ok response without the match: body echo.
func AnsweredFromCatalog(req LookupRequest, r Result) bool {
	return req.Match == MatchBody && r.Response.Status == protocol.StatusOK && r.Response.Metadata["match"] != MatchBody
}

// TokenResolver answers the capability token to send to host (host:port), or
// "" for none. A crawl reaches hosts the user never named, so the token is
// asked for per host and never assumed.
type TokenResolver interface {
	Token(host string) string
}

// TokenResolverFunc adapts a function to TokenResolver.
type TokenResolverFunc func(host string) string

// Token calls f.
func (f TokenResolverFunc) Token(host string) string { return f(host) }
