// Package fetch provides shared Mark Protocol fetch logic for CLI and TUI clients.
package fetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/url"
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

// Result holds a fetch response and metadata about how it was served.
type Result struct {
	Response  protocol.Response
	FromCache bool
}

// Options configures client behavior.
type Options struct {
	Cache    *cache.Cache
	Insecure bool
}

// Client manages QUIC connections and fetches Mark Protocol documents.
type Client struct {
	opts    Options
	tlsConf *tls.Config
	mu      sync.Mutex
	conns   map[string]*quic.Conn
}

// NewClient creates a new fetch client with the given options.
func NewClient(opts Options) *Client {
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

// getConn returns a pooled connection to host, or dials a new one.
func (c *Client) getConn(host string) (*quic.Conn, error) {
	c.mu.Lock()
	conn, ok := c.conns[host]
	c.mu.Unlock()

	if ok {
		return conn, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// removeConn removes a stale connection from the pool.
func (c *Client) removeConn(host string) {
	c.mu.Lock()
	delete(c.conns, host)
	c.mu.Unlock()
}

// Fetch retrieves a document from a Mark Protocol server.
func (c *Client) Fetch(host, path, verb string) (Result, error) {
	conn, err := c.getConn(host)
	if err != nil {
		return Result{}, err
	}

	result, err := c.fetchOnConn(conn, host, path, verb)
	if err != nil {
		// Connection may be stale — redial once and retry.
		c.removeConn(host)
		conn, dialErr := c.getConn(host)
		if dialErr != nil {
			return Result{}, err
		}
		return c.fetchOnConn(conn, host, path, verb)
	}
	return result, nil
}

func (c *Client) fetchOnConn(conn *quic.Conn, host, path, verb string) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("open stream: %w", err)
	}
	defer stream.Close()

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

	if _, err := req.WriteTo(stream); err != nil {
		return Result{}, fmt.Errorf("send request: %w", err)
	}
	// Send FIN so the server knows the request is complete.
	stream.Close()

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}

	fromCache := false
	if resp.Status == protocol.StatusNotModified && cached != nil && cached.Response.Status == protocol.StatusOK {
		resp = cached.Response
		fromCache = true
	}

	if c.opts.Cache != nil && resp.Status == protocol.StatusOK {
		if err := c.opts.Cache.Put(host, path, verb, resp); err != nil {
			log.Printf("cache write: %v", err)
		}
	}

	return Result{Response: resp, FromCache: fromCache}, nil
}
