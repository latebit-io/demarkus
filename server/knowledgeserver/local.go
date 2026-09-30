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
// as a QUIC stream with that SNI would be served, plus any protocol.Grant on
// ctx. It returns when ctx ends, as a cancelled QUIC client would.
func (s *Server) Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	endpoint, err := s.worlds.Router().Lookup(authority)
	if err != nil {
		return protocol.Response{}, err
	}
	var request bytes.Buffer
	if _, err := req.WriteTo(&request); err != nil {
		return protocol.Response{}, fmt.Errorf("encode request: %w", err)
	}
	// Buffers on both sides, so the only wait is the world's own work; a
	// commit keeps sealing past cancellation and must not hold the caller.
	stream := &localStream{Reader: &request}
	served := make(chan struct{})
	go func() {
		defer close(served)
		endpoint.ServeStream(ctx, localAddr{}, stream)
	}()
	select {
	case <-ctx.Done():
		return protocol.Response{}, ctx.Err()
	case <-served:
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
