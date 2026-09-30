package knowledgeserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
)

// ErrUnknownAuthority is Exchange's answer for an authority no live world
// routes.
var ErrUnknownAuthority = snirouter.ErrUnknownAuthority

// Routes reports whether a live world serves authority right now.
func (s *Server) Routes(authority string) bool {
	_, err := s.worlds.Router().Lookup(authority)
	return err == nil
}

// Exchange answers req through the world that routes authority, in process,
// exactly as a QUIC stream with that SNI: admission, rate limits, auth and
// the handler included. ctx bounds it; stream deadlines are no-ops here.
func (s *Server) Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	endpoint, err := s.worlds.Router().Lookup(authority)
	if err != nil {
		return protocol.Response{}, err
	}
	// The request is one buffer the handler reads to EOF and the response
	// one write, so nothing blocks: no pipe, no goroutine.
	var request bytes.Buffer
	if _, err := req.WriteTo(&request); err != nil {
		return protocol.Response{}, fmt.Errorf("encode request: %w", err)
	}
	stream := &localStream{Reader: &request}
	endpoint.ServeStream(ctx, localAddr{}, stream)
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	resp, err := protocol.ParseResponse(&stream.response)
	if err != nil {
		return protocol.Response{}, fmt.Errorf("read response: %w", err)
	}
	return resp, nil
}

// localStream is the server's end of an in-process exchange: the encoded
// request to read, a buffer to answer into. Deadlines are accepted and
// ignored; ctx bounds the exchange instead.
type localStream struct {
	io.Reader
	response bytes.Buffer
}

func (l *localStream) Write(p []byte) (int, error)    { return l.response.Write(p) }
func (*localStream) Close() error                     { return nil }
func (*localStream) SetReadDeadline(time.Time) error  { return nil }
func (*localStream) SetWriteDeadline(time.Time) error { return nil }

// localAddr is the remote of an in-process stream; the rate limiter keys on
// it, so local callers share one bucket, as one broker pod does over QUIC.
type localAddr struct{}

func (localAddr) Network() string { return "local" }
func (localAddr) String() string  { return "local" }
