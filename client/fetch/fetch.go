// Package fetch provides shared Mark Protocol client logic for CLI and TUI clients.
package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/client/internal/cache"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// ParseMarkURL parses a mark:// URL and returns the host (with default port) and path.
func ParseMarkURL(raw string) (host, path string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "mark" {
		return "", "", fmt.Errorf("unsupported scheme: %s (expected mark://)", u.Scheme)
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" {
		return "", "", fmt.Errorf("invalid URL: authority host is required")
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(protocol.DefaultPort)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("invalid URL: port %q must be between 1 and 65535", port)
	}
	host = net.JoinHostPort(hostname, port)
	path = u.Path
	if path == "" {
		path = "/"
	}
	return host, path, nil
}

// Result holds a response and metadata about how it was served.
type Result struct {
	Response  protocol.Response
	FromCache bool
}

// Endpoint separates a logical Mark authority from its network route.
// DialAddress opens the socket; ServerName drives TLS SNI and verification.
type Endpoint struct {
	DialAddress string
	ServerName  string
}

// Options configures client behavior.
type Options struct {
	Cache          *cache.Cache
	Insecure       bool
	DialTimeout    time.Duration
	RequestTimeout time.Duration
	// Endpoints optionally overrides transport routing by normalized logical
	// authority (host:port). URLs, caches, tokens, and connection pooling remain
	// keyed by that authority.
	Endpoints map[string]Endpoint
	// KeepAlivePeriod controls QUIC keep-alive PING cadence on pooled
	// connections. Long-lived consumers (TUI, MCP servers, federation
	// crawlers) keep one connection per host across many requests; if
	// the connection sits idle long enough for the NAT path or server-
	// side idle timer to drop it, the next request silently waits for
	// RequestTimeout before transient-failure retry kicks in. With
	// keep-alive set, quic-go sends PING frames at this interval
	// whenever the connection has been idle, holding the path open
	// across typical 30s+ NAT timeouts. Default 25s (under common NAT
	// thresholds, well under quic-go's 30s default idle timeout). Set
	// to a negative value to disable.
	KeepAlivePeriod time.Duration
}

func (o *Options) applyDefaults() {
	if o.DialTimeout == 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 10 * time.Second
	}
	if o.KeepAlivePeriod == 0 {
		o.KeepAlivePeriod = 25 * time.Second
	}
}

// Client manages QUIC connections and performs Mark Protocol operations.
type Client struct {
	opts     Options
	tlsConf  *tls.Config
	quicConf *quic.Config
	mu       sync.Mutex
	conns    map[string]*quic.Conn
}

// NewClient creates a new client with the given options.
func NewClient(opts Options) *Client {
	opts.applyDefaults()
	opts.Endpoints = maps.Clone(opts.Endpoints)
	qc := &quic.Config{}
	// Negative KeepAlivePeriod disables; positive applies.
	if opts.KeepAlivePeriod > 0 {
		qc.KeepAlivePeriod = opts.KeepAlivePeriod
	}
	return &Client{
		opts: opts,
		tlsConf: &tls.Config{
			InsecureSkipVerify: opts.Insecure,
			NextProtos:         []string{protocol.ALPN},
		},
		quicConf: qc,
		conns:    make(map[string]*quic.Conn),
	}
}

// Close closes all pooled connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for host, conn := range c.conns {
		_ = conn.CloseWithError(0, "")
		delete(c.conns, host)
	}
}

// Fetch retrieves a document from a Mark Protocol server.
// If token is non-empty, it is sent as the auth metadata for read access to private paths.
func (c *Client) Fetch(host, path, token string) (Result, error) {
	return c.cachedRequest(host, path, token, protocol.VerbFetch)
}

// FetchConditional is Fetch with an explicit if-none-match etag, for callers
// that track document freshness themselves (e.g. the graph seeder, whose
// authenticated fetches skip the disk cache so the built-in conditional path
// never fires). A not-modified status returns with an empty body. An empty
// etag degrades to a plain Fetch.
func (c *Client) FetchConditional(host, path, token, etag string) (Result, error) {
	if etag == "" {
		return c.Fetch(host, path, token)
	}
	return c.cachedRequestMeta(host, path, token, protocol.VerbFetch, map[string]string{"if-none-match": etag})
}

// ListOptions carries optional parameters for a LIST request.
type ListOptions struct {
	// IncludeArchived asks the server to include archived documents (and
	// directories that contain only archived documents) in the listing.
	// Default false: archived entries are hidden.
	IncludeArchived bool
	// Cursor continues the same directory and archive-mode listing.
	Cursor string
	// PageSize caps entries returned; zero uses the server default.
	PageSize int
}

// ErrListCompletenessUnknown means a LIST response predates machine-readable
// pagination metadata. Callers cannot infer completeness from its body.
var ErrListCompletenessUnknown = errors.New("LIST response completeness is unknown")

// ListPageMetadata is the machine-readable state of one successful LIST page.
type ListPageMetadata struct {
	Entries    int
	Complete   bool
	NextCursor string
}

// ParseListPageMetadata validates a successful LIST response's pagination state.
func ParseListPageMetadata(resp protocol.Response) (ListPageMetadata, error) {
	if resp.Status != protocol.StatusOK {
		return ListPageMetadata{}, fmt.Errorf("LIST returned status %q", resp.Status)
	}
	entries, err := strconv.Atoi(resp.Metadata["entries"])
	if err != nil || entries < 0 || entries > protocol.MaxListPageSize {
		return ListPageMetadata{}, errors.New("LIST response has invalid entries metadata")
	}
	if len(resp.Body) > protocol.MaxBodyLength {
		return ListPageMetadata{}, errors.New("LIST response body exceeds limit")
	}
	rawComplete, ok := resp.Metadata["complete"]
	if !ok {
		return ListPageMetadata{}, ErrListCompletenessUnknown
	}
	complete, err := strconv.ParseBool(rawComplete)
	if err != nil || (rawComplete != "true" && rawComplete != "false") {
		return ListPageMetadata{}, errors.New("LIST response has invalid complete metadata")
	}
	next := resp.Metadata["next-cursor"]
	if complete && next != "" {
		return ListPageMetadata{}, errors.New("complete LIST response contains next-cursor")
	}
	if !complete && next == "" {
		return ListPageMetadata{}, errors.New("incomplete LIST response is missing next-cursor")
	}
	if !complete && entries == 0 {
		return ListPageMetadata{}, errors.New("incomplete LIST response made no progress")
	}
	return ListPageMetadata{Entries: entries, Complete: complete, NextCursor: next}, nil
}

// List retrieves a directory listing from a Mark Protocol server.
// If token is non-empty, it is sent as the auth metadata for read access to private paths.
func (c *Client) List(host, path, token string) (Result, error) {
	return c.ListWithOptions(host, path, token, ListOptions{})
}

// ListWithOptions is List with explicit LIST options (e.g. IncludeArchived).
func (c *Client) ListWithOptions(host, path, token string, opts ListOptions) (Result, error) {
	if opts.PageSize < 0 || opts.PageSize > protocol.MaxListPageSize {
		return Result{}, fmt.Errorf("LIST page size must be between 1 and %d, or 0 for the server default", protocol.MaxListPageSize)
	}
	extra := make(map[string]string)
	if opts.IncludeArchived {
		extra["include-archived"] = "true"
	}
	if opts.Cursor != "" {
		extra["cursor"] = opts.Cursor
	}
	if opts.PageSize > 0 {
		extra["page-size"] = strconv.Itoa(opts.PageSize)
	}
	if len(extra) == 0 {
		extra = nil
	}
	return c.cachedRequestMeta(host, path, token, protocol.VerbList, extra)
}

// Versions retrieves the version history of a document.
// If token is non-empty, it is sent as the auth metadata for read access to private paths.
func (c *Client) Versions(host, path, token string) (Result, error) {
	req := protocol.Request{Verb: protocol.VerbVersions, Path: path, Metadata: make(map[string]string)}
	if token != "" {
		req.Metadata["auth"] = token
	}
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// Publish creates or updates a document on a Mark Protocol server.
// If token is non-empty, it is sent as the auth metadata for capability-based auth.
// expectedVersion controls optimistic concurrency:
//   - < 0: no check (server accepts unconditionally)
//   - 0: create-only (server rejects if document already exists)
//   - > 0: update-only (server rejects if current version doesn't match)
func (c *Client) Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (Result, error) {
	req := protocol.Request{Verb: protocol.VerbPublish, Path: path, Metadata: make(map[string]string), Body: body}
	maps.Copy(req.Metadata, meta)
	if token != "" {
		req.Metadata["auth"] = token
	}
	if expectedVersion >= 0 {
		req.Metadata["expected-version"] = strconv.Itoa(expectedVersion)
	}
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// Append adds content to the end of an existing document.
// expectedVersion is required and must be >= 1 (the document must already exist).
// If token is non-empty, it is sent as the auth metadata for capability-based auth.
func (c *Client) Append(host, path, body, token string, expectedVersion int, meta map[string]string) (Result, error) {
	if expectedVersion < 1 {
		return Result{}, fmt.Errorf("APPEND requires expected-version >= 1, got %d", expectedVersion)
	}
	if body == "" {
		return Result{}, fmt.Errorf("APPEND requires a non-empty body")
	}
	req := protocol.Request{Verb: protocol.VerbAppend, Path: path, Metadata: make(map[string]string), Body: body}
	maps.Copy(req.Metadata, meta)
	if token != "" {
		req.Metadata["auth"] = token
	}
	req.Metadata["expected-version"] = strconv.Itoa(expectedVersion)
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// LookupOptions configures a LOOKUP request. Zero values are omitted from the
// request so the server applies its own defaults.
type LookupOptions struct {
	Filter string // comma-separated key=value predicates
	Limit  int    // max results; <= 0 lets the server choose
}

// Lookup queries a server's catalog for documents matching a subject under
// scope, returning an importance-ranked markdown table. query is required.
// If token is non-empty it is sent so read-auth-gated documents are included.
func (c *Client) Lookup(host, scope, query, token string, opts LookupOptions) (Result, error) {
	return c.LookupContext(context.Background(), host, scope, query, token, opts)
}

// LookupContext is Lookup with caller cancellation propagated through dialing,
// retries, and stream I/O.
func (c *Client) LookupContext(ctx context.Context, host, scope, query, token string, opts LookupOptions) (Result, error) {
	if query == "" {
		return Result{}, fmt.Errorf("LOOKUP requires a non-empty query")
	}
	req := protocol.Request{Verb: protocol.VerbLookup, Path: scope, Metadata: map[string]string{"query": query}}
	if opts.Filter != "" {
		req.Metadata["filter"] = opts.Filter
	}
	if opts.Limit > 0 {
		req.Metadata["limit"] = strconv.Itoa(opts.Limit)
	}
	if token != "" {
		req.Metadata["auth"] = token
	}
	return c.doWithRetryContext(ctx, host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConnContext(ctx, conn, req)
	})
}

// Archive marks a document as archived on a Mark Protocol server.
func (c *Client) Archive(host, path, token string) (Result, error) {
	req := protocol.Request{Verb: protocol.VerbArchive, Path: path, Metadata: make(map[string]string)}
	if token != "" {
		req.Metadata["auth"] = token
	}
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// cachedRequest handles FETCH and LIST with conditional caching.
func (c *Client) cachedRequest(host, path, token, verb string) (Result, error) {
	return c.cachedRequestMeta(host, path, token, verb, nil)
}

// cachedRequestMeta is cachedRequest with extra request metadata (e.g. LIST
// options). When extra is non-empty the response cache is bypassed: the cache
// key is (host, path, verb) and does not encode the extra metadata, so a
// cached option-free response must not satisfy an option-bearing request, nor
// the reverse.
func (c *Client) cachedRequestMeta(host, path, token, verb string, extra map[string]string) (Result, error) {
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		req := protocol.Request{Verb: verb, Path: path, Metadata: make(map[string]string)}

		if token != "" {
			req.Metadata["auth"] = token
		}
		maps.Copy(req.Metadata, extra)

		// Skip cache for authenticated requests (to avoid persisting private
		// content to disk) and for option-bearing requests (the cache key does
		// not encode the extra metadata).
		useCache := c.opts.Cache != nil && token == "" && len(extra) == 0

		var cached *cache.Entry
		if useCache {
			cached, _ = c.opts.Cache.Get(host, path, verb)
			if cached != nil {
				if etag := cached.Response.Metadata["etag"]; etag != "" {
					req.Metadata["if-none-match"] = etag
				}
				if mod := cached.Response.Metadata["modified"]; mod != "" {
					req.Metadata["if-modified-since"] = mod
				}
			}
		}

		result, err := c.requestOnConn(conn, req)
		if err != nil {
			return Result{}, err
		}

		if result.Response.Status == protocol.StatusNotModified && cached != nil && cached.Response.Status == protocol.StatusOK {
			return Result{Response: cached.Response, FromCache: true}, nil
		}

		if useCache && result.Response.Status == protocol.StatusOK {
			if err := c.opts.Cache.Put(host, path, verb, result.Response); err != nil {
				log.Printf("[WARN] cache write: %v", err)
			}
		}

		return result, nil
	})
}

// requestOnConn opens a stream, sends a request, and reads the response.
func (c *Client) requestOnConn(conn *quic.Conn, req protocol.Request) (Result, error) {
	return c.requestOnConnContext(context.Background(), conn, req)
}

func (c *Client) requestOnConnContext(ctx context.Context, conn *quic.Conn, req protocol.Request) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("open stream: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		stream.CancelRead(0)
		stream.CancelWrite(0)
	})
	defer stopCancel()

	if _, err := req.WriteTo(stream); err != nil {
		stream.CancelWrite(0)
		stream.CancelRead(0)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("send request: %w", err)
	}
	if err := stream.Close(); err != nil {
		stream.CancelRead(0)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("close request stream: %w", err)
	}

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		stream.CancelRead(0)
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, fmt.Errorf("read response: %w", err)
	}

	return Result{Response: resp}, nil
}

// doWithRetry retries transient failures up to 5 times with a fixed 100ms delay.
func (c *Client) doWithRetry(host string, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	return c.doWithRetryContext(context.Background(), host, fn)
}

func (c *Client) doWithRetryContext(ctx context.Context, host string, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	const maxRetries = 5
	const retryDelay = 100 * time.Millisecond

	var lastErr error
	for attempt := range maxRetries {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		conn, err := c.getConnContext(ctx, host)
		if err != nil {
			if attempt < maxRetries-1 && isTransientError(err) {
				if err := waitForRetry(ctx, retryDelay); err != nil {
					return Result{}, err
				}
				c.removeConn(host)
				continue
			}
			return Result{}, err
		}

		result, err := fn(conn)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}

		lastErr = err
		if attempt < maxRetries-1 && isTransientError(err) {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return Result{}, err
			}
			c.removeConn(host)
			continue
		}

		return Result{}, err
	}

	return Result{}, lastErr
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) getConnContext(ctx context.Context, host string) (*quic.Conn, error) {
	c.mu.Lock()
	conn, ok := c.conns[host]
	c.mu.Unlock()

	if ok {
		if conn.Context().Err() != nil {
			c.removeConn(host)
		} else {
			return conn, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.DialTimeout)
	defer cancel()

	endpoint := Endpoint{DialAddress: host}
	if configured, ok := c.opts.Endpoints[host]; ok {
		endpoint = configured
		if endpoint.DialAddress == "" {
			endpoint.DialAddress = host
		}
	}

	// Clone TLS config and set ServerName for routing and certificate validation.
	tlsConf := c.tlsConf.Clone()
	tlsConf.ServerName = endpoint.ServerName
	if tlsConf.ServerName == "" {
		tlsConf.ServerName = authorityHostname(host)
	}
	conn, err := quic.DialAddr(ctx, endpoint.DialAddress, tlsConf, c.quicConf)
	if err != nil {
		if endpoint.DialAddress != host {
			return nil, fmt.Errorf("dial %s via %s: %w", host, endpoint.DialAddress, err)
		}
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	c.mu.Lock()
	if existing, ok := c.conns[host]; ok && existing.Context().Err() == nil {
		// Another goroutine dialed and stored a connection while we were dialing.
		// Use theirs; close ours.
		c.mu.Unlock()
		_ = conn.CloseWithError(0, "")
		return existing, nil
	}
	c.conns[host] = conn
	c.mu.Unlock()

	return conn, nil
}

func authorityHostname(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		return host
	}
	return authority
}

func (c *Client) removeConn(host string) {
	c.mu.Lock()
	delete(c.conns, host)
	c.mu.Unlock()
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if isTimeoutError(err) || isTemporaryError(err) {
		return true
	}
	errStr := err.Error()
	switch {
	case errStr == "EOF":
		return true
	case strings.Contains(errStr, "no recent network activity"):
		return true
	case strings.Contains(errStr, "connection refused"):
		return true
	case strings.Contains(errStr, "connection reset"):
		return true
	}
	return false
}

// isTimeoutError reports whether err (or anything it wraps) is a timeout.
// It must use errors.As, not a bare type assertion: every error returned from
// this package is wrapped (e.g. fmt.Errorf("open stream: %w", …)), and a
// *fmt.wrapError does not itself implement Timeout(). A bare assertion misses
// the wrapped context.DeadlineExceeded, so a timed-out OpenStreamSync would be
// classed non-transient and doWithRetry would never evict+redial the dead
// pooled connection — wedging every subsequent request on that host.
func isTimeoutError(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

func isTemporaryError(err error) bool {
	var te interface{ Temporary() bool }
	return errors.As(err, &te) && te.Temporary()
}
