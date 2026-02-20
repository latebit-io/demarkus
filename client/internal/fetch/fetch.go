// Package fetch provides shared Mark Protocol client logic for CLI and TUI clients.
package fetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/protocol"
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
	host = u.Host
	if u.Port() == "" {
		host = fmt.Sprintf("%s:%d", u.Hostname(), protocol.DefaultPort)
	}
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

// Options configures client behavior.
type Options struct {
	Cache          *cache.Cache
	Insecure       bool
	DialTimeout    time.Duration
	RequestTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.DialTimeout == 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 10 * time.Second
	}
}

// Client manages QUIC connections and performs Mark Protocol operations.
type Client struct {
	opts    Options
	tlsConf *tls.Config
	mu      sync.Mutex
	conns   map[string]*quic.Conn
}

// NewClient creates a new client with the given options.
func NewClient(opts Options) *Client {
	opts.applyDefaults()
	return &Client{
		opts: opts,
		tlsConf: &tls.Config{
			InsecureSkipVerify: opts.Insecure,
			NextProtos:         []string{protocol.ALPN},
		},
		conns: make(map[string]*quic.Conn),
	}
}

// Close closes all pooled connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for host, conn := range c.conns {
		conn.CloseWithError(0, "")
		delete(c.conns, host)
	}
}

// Fetch retrieves a document from a Mark Protocol server.
func (c *Client) Fetch(host, path string) (Result, error) {
	return c.cachedRequest(host, path, protocol.VerbFetch)
}

// List retrieves a directory listing from a Mark Protocol server.
func (c *Client) List(host, path string) (Result, error) {
	return c.cachedRequest(host, path, protocol.VerbList)
}

// Versions retrieves the version history of a document.
func (c *Client) Versions(host, path string) (Result, error) {
	req := protocol.Request{Verb: protocol.VerbVersions, Path: path, Metadata: make(map[string]string)}
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// Write creates or updates a document on a Mark Protocol server.
// If token is non-empty, it is sent as the auth metadata for capability-based auth.
func (c *Client) Write(host, path, body, token string) (Result, error) {
	req := protocol.Request{Verb: protocol.VerbWrite, Path: path, Metadata: make(map[string]string), Body: body}
	if token != "" {
		req.Metadata["auth"] = token
	}
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		return c.requestOnConn(conn, req)
	})
}

// cachedRequest handles FETCH and LIST with conditional caching.
func (c *Client) cachedRequest(host, path, verb string) (Result, error) {
	return c.doWithRetry(host, func(conn *quic.Conn) (Result, error) {
		req := protocol.Request{Verb: verb, Path: path, Metadata: make(map[string]string)}

		var cached *cache.Entry
		if c.opts.Cache != nil {
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

		if c.opts.Cache != nil && result.Response.Status == protocol.StatusOK {
			if err := c.opts.Cache.Put(host, path, verb, result.Response); err != nil {
				log.Printf("[WARN] cache write: %v", err)
			}
		}

		return result, nil
	})
}

// requestOnConn opens a stream, sends a request, and reads the response.
func (c *Client) requestOnConn(conn *quic.Conn, req protocol.Request) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.opts.RequestTimeout)
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("open stream: %w", err)
	}
	defer stream.Close()

	if _, err := req.WriteTo(stream); err != nil {
		return Result{}, fmt.Errorf("send request: %w", err)
	}
	stream.Close()

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}

	return Result{Response: resp}, nil
}

// doWithRetry retries transient failures up to 3 times with exponential backoff + jitter.
func (c *Client) doWithRetry(host string, fn func(conn *quic.Conn) (Result, error)) (Result, error) {
	const maxRetries = 3
	const baseBackoff = 100 * time.Millisecond

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		conn, err := c.getConn(host)
		if err != nil {
			if attempt < maxRetries-1 && isTransientError(err) {
				backoff := baseBackoff * time.Duration(1<<uint(attempt))
				jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
				time.Sleep(backoff + jitter)
				c.removeConn(host)
				continue
			}
			return Result{}, err
		}

		result, err := fn(conn)
		if err == nil {
			return result, nil
		}

		lastErr = err
		if attempt < maxRetries-1 && isTransientError(err) {
			backoff := baseBackoff * time.Duration(1<<uint(attempt))
			jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
			time.Sleep(backoff + jitter)
			c.removeConn(host)
			continue
		}

		return Result{}, err
	}

	return Result{}, lastErr
}

func (c *Client) getConn(host string) (*quic.Conn, error) {
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

	ctx, cancel := context.WithTimeout(context.Background(), c.opts.DialTimeout)
	defer cancel()

	conn, err := quic.DialAddr(ctx, host, c.tlsConf, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", host, err)
	}

	c.mu.Lock()
	c.conns[host] = conn
	c.mu.Unlock()

	return conn, nil
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

func isTimeoutError(err error) bool {
	type timeoutError interface {
		Timeout() bool
	}
	te, ok := err.(timeoutError)
	return ok && te.Timeout()
}

func isTemporaryError(err error) bool {
	type temporaryError interface {
		Temporary() bool
	}
	te, ok := err.(temporaryError)
	return ok && te.Temporary()
}
