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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

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

// ResponseCache holds cacheable reads, keyed by host, path and verb. A miss is
// a nil entry with no error. The client caches only tokenless reads without
// options, since neither a token nor an option is part of the key.
type ResponseCache interface {
	Get(host, path, verb string) (*CachedResponse, error)
	Put(host, path, verb string, resp protocol.Response) error
}

// CachedResponse is a stored response and when it was stored.
type CachedResponse struct {
	Response protocol.Response
	CachedAt time.Time
}

// Options configures client behavior.
type Options struct {
	Cache          ResponseCache // nil disables caching
	Insecure       bool
	DialTimeout    time.Duration
	RequestTimeout time.Duration
	// Endpoints optionally overrides transport routing by normalized logical
	// authority (host:port). URLs, caches, tokens, and connection pooling remain
	// keyed by that authority.
	Endpoints map[string]Endpoint
	// KeepAlivePeriod is the QUIC PING cadence on idle pooled connections, so a
	// NAT or server idle timer never drops one between requests. Default 25s,
	// under common NAT timeouts; negative disables.
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
	// inflight counts requests per connection; draining holds evicted
	// connections until their last request releases them.
	inflight map[*quic.Conn]int
	draining map[*quic.Conn]bool
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
		inflight: make(map[*quic.Conn]int),
		draining: make(map[*quic.Conn]bool),
	}
}

// Close closes all pooled connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for host, conn := range c.conns {
		closeConn(conn)
		delete(c.conns, host)
	}
	for conn := range c.draining {
		closeConn(conn)
		delete(c.draining, conn)
	}
}

// closeConn closes best effort: the peer may already be gone, and no caller can act on it.
func closeConn(conn *quic.Conn) {
	_ = conn.CloseWithError(0, "")
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

// LOOKUP match modes. A server without body match answers from the
// catalog; AnsweredFromCatalog detects that.
const (
	MatchCatalog = protocol.MatchCatalog
	MatchBody    = protocol.MatchBody
)

// CatalogFallbackNote is what surfaces report when body match was requested
// and the server answered from the catalog instead.
const CatalogFallbackNote = "server answered from the catalog (no body match); an empty table is not evidence of absence"

// readRequest is one cacheable read: FETCH or LIST.
type readRequest struct {
	host, path, token, verb string
	extra                   map[string]string // request options; any of them bypasses the cache
}

// cachedRead serves a read through the response cache when it may.
func (c *Client) cachedRead(ctx context.Context, r readRequest) (Result, error) {
	host, path, token, verb, extra := r.host, r.path, r.token, r.verb, r.extra
	return c.doWithRetryContext(ctx, host, func(conn *quic.Conn) (Result, error) {
		req := newRequest(verb, path, token, extra)

		// Skip cache for authenticated requests (to avoid persisting private
		// content to disk) and for option-bearing requests (the cache key does
		// not encode the extra metadata).
		useCache := c.opts.Cache != nil && token == "" && len(extra) == 0

		var cached *CachedResponse
		if useCache {
			// An unreadable cache entry is a miss: the request goes out unconditional.
			var cacheErr error
			if cached, cacheErr = c.opts.Cache.Get(host, path, verb); cacheErr != nil {
				log.Printf("[WARN] cache read %s %s%s: %v", verb, host, path, cacheErr)
			}
			if cached != nil {
				if etag := cached.Response.Metadata["etag"]; etag != "" {
					req.Metadata["if-none-match"] = etag
				}
				if mod := cached.Response.Metadata["modified"]; mod != "" {
					req.Metadata["if-modified-since"] = mod
				}
			}
		}

		result, err := c.requestOnConnContext(ctx, conn, req)
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

// requestOnConnContext opens a stream, sends a request, and reads the response.
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

	// From the first written byte on, the server may have acted on the request.
	if n, err := req.WriteTo(stream); err != nil {
		stream.CancelWrite(0)
		stream.CancelRead(0)
		if ctx.Err() != nil {
			err = ctx.Err()
		} else {
			err = fmt.Errorf("send request: %w", err)
		}
		// The request goes out in one write; zero accepted bytes means nothing was sent.
		if n == 0 {
			return Result{}, err
		}
		return Result{}, &sentError{cause: err}
	}
	if err := stream.Close(); err != nil {
		stream.CancelRead(0)
		if ctx.Err() != nil {
			return Result{}, &sentError{cause: ctx.Err()}
		}
		return Result{}, &sentError{cause: fmt.Errorf("close request stream: %w", err)}
	}

	resp, err := protocol.ParseResponse(responseReader(ctx, stream))
	if err != nil {
		stream.CancelRead(0)
		if ctx.Err() != nil {
			return Result{}, &sentError{cause: ctx.Err()}
		}
		return Result{}, &sentError{cause: fmt.Errorf("read response: %w", err)}
	}

	return Result{Response: resp}, nil
}

// sentError wraps a failure that happened after request bytes left the client.
type sentError struct{ cause error }

func (e *sentError) Error() string   { return e.cause.Error() }
func (e *sentError) Unwrap() error   { return e.cause }
func (e *sentError) Is(t error) bool { return t == protocol.ErrOutcomeUnknown }

// doWriteContext is doWithRetryContext for non idempotent verbs: dial and
// open stream failures retry, anything after the request was sent does not.
func (c *Client) doWriteContext(ctx context.Context, host string, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	return c.retry(ctx, host, false, fn)
}

// doWithRetryContext retries transient failures up to 5 times with a fixed 100ms delay.
func (c *Client) doWithRetryContext(ctx context.Context, host string, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	return c.retry(ctx, host, true, fn)
}

func (c *Client) retry(ctx context.Context, host string, resend bool, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	const maxRetries = 5
	const retryDelay = 100 * time.Millisecond

	var lastErr error
	for attempt := range maxRetries {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		// A failed dial stored nothing; whatever is pooled belongs to someone else.
		conn, err := c.acquire(ctx, host)
		if err != nil {
			if attempt < maxRetries-1 && isTransientError(err) {
				if err := waitForRetry(ctx, retryDelay); err != nil {
					return Result{}, err
				}
				continue
			}
			return Result{}, err
		}

		result, err := fn(conn)
		unknown := !resend && errors.Is(err, protocol.ErrOutcomeUnknown)
		if err != nil && (unknown || isTransientError(err)) {
			c.evict(host, conn)
		}
		c.release(conn)
		if err == nil {
			return result, nil
		}
		// Checked before caller cancellation, which would otherwise read as a
		// definite failure and invite a resend of a write that may have landed.
		if unknown {
			return Result{}, err
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}

		lastErr = err
		if attempt < maxRetries-1 && isTransientError(err) {
			if err := waitForRetry(ctx, retryDelay); err != nil {
				return Result{}, err
			}
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
		if conn.Context().Err() == nil {
			return conn, nil
		}
		c.evict(host, conn)
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
		closeConn(conn)
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

// acquire returns the pooled connection for host, dialing if needed, and counts
// the caller as a user until release.
func (c *Client) acquire(ctx context.Context, host string) (*quic.Conn, error) {
	for {
		conn, err := c.getConnContext(ctx, host)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		// Still pooled and not retiring: an idle evict closes without marking draining.
		if c.conns[host] == conn && !c.draining[conn] {
			c.inflight[conn]++
			c.mu.Unlock()
			return conn, nil
		}
		// Evicted between lookup and here; look again.
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// release ends one use of conn and closes it if it was evicted and is now idle.
func (c *Client) release(conn *quic.Conn) {
	c.mu.Lock()
	c.inflight[conn]--
	idle := c.inflight[conn] <= 0
	if idle {
		delete(c.inflight, conn)
	}
	closing := idle && c.draining[conn]
	if closing {
		delete(c.draining, conn)
	}
	c.mu.Unlock()
	if closing {
		closeConn(conn)
	}
}

// evict retires conn: no new users, closed after its last release. The pool
// entry goes only if it still is conn, so a replacement is never dropped.
func (c *Client) evict(host string, conn *quic.Conn) {
	c.mu.Lock()
	if c.conns[host] == conn {
		delete(c.conns, host)
	}
	idle := c.inflight[conn] <= 0
	if !idle {
		c.draining[conn] = true
	}
	c.mu.Unlock()
	if idle {
		closeConn(conn)
	}
}

func isTransientError(err error) bool {
	if err == nil || errors.Is(err, ErrResponseBudget) {
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

// isTimeoutError must use errors.As: every error here is wrapped, and a bare
// assertion would miss a timed out OpenStreamSync, leave the dead pooled
// connection in place and wedge every later request to that host.
func isTimeoutError(err error) bool {
	var te interface{ Timeout() bool }
	return errors.As(err, &te) && te.Timeout()
}

func isTemporaryError(err error) bool {
	var te interface{ Temporary() bool }
	return errors.As(err, &te) && te.Temporary()
}
