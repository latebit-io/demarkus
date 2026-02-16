// Package fetch provides shared Mark Protocol fetch logic for CLI and TUI clients.
package fetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// Result holds a fetch response and metadata about how it was served.
type Result struct {
	Response  protocol.Response
	FromCache bool
}

// Fetch retrieves a document from a Mark Protocol server.
// If c is non-nil, cache conditional headers are sent and successful responses are cached.
func Fetch(host, path, verb string, c *cache.Cache) (Result, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{protocol.ALPN},
	}

	conn, err := quic.DialAddr(context.Background(), host, tlsConf, nil)
	if err != nil {
		return Result{}, fmt.Errorf("dial %s: %w", host, err)
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return Result{}, fmt.Errorf("open stream: %w", err)
	}

	req := protocol.Request{Verb: verb, Path: path, Metadata: make(map[string]string)}

	var cached *cache.Entry
	if c != nil {
		cached, _ = c.Get(host, path, verb)
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
	stream.Close()

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}

	fromCache := false
	if resp.Status == protocol.StatusNotModified && cached != nil {
		resp = cached.Response
		fromCache = true
	}

	if c != nil && resp.Status == protocol.StatusOK {
		if err := c.Put(host, path, verb, resp); err != nil {
			log.Printf("cache write: %v", err)
		}
	}

	return Result{Response: resp, FromCache: fromCache}, nil
}
