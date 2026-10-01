package knowledgeserver

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
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
// under a QUIC stream's controls plus any protocol.Grant on ctx. A cancelled
// ctx ends reads at once; a commit under way seals first and is reported.
func (s *Server) Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	exchanger, err := s.exchanger(authority)
	if err != nil {
		return protocol.Response{}, err
	}
	return exchanger.Exchange(ctx, localAddr{}, req)
}

// Watch serves a WATCH request in process through the world that routes
// authority, under a stream's controls plus any protocol.Grant on ctx. The
// returned conn carries the watch's blocks until ctx ends or it is closed.
func (s *Server) Watch(ctx context.Context, authority string, req protocol.Request) (net.Conn, error) {
	if req.Verb != protocol.VerbWatch {
		return nil, fmt.Errorf("in-process watch of %s: verb %s", authority, req.Verb)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exchanger, err := s.exchanger(authority)
	if err != nil {
		return nil, err
	}
	client, server := net.Pipe()
	watchCtx, cancel := context.WithCancel(ctx)
	served := make(chan struct{})
	go func() {
		defer close(served)
		exchanger.Watch(watchCtx, localAddr{}, req, server)
	}()
	return &localWatch{Conn: client, cancel: cancel, served: served}, nil
}

func (s *Server) exchanger(authority string) (worldruntime.Exchanger, error) {
	endpoint, err := s.worlds.Router().Lookup(authority)
	if err != nil {
		return nil, err
	}
	exchanger, ok := endpoint.(worldruntime.Exchanger)
	if !ok {
		return nil, fmt.Errorf("authority %s: endpoint %T serves streams only", authority, endpoint)
	}
	return exchanger, nil
}

// localWatch is the reading end of an in-process watch. Close ends the watch
// and waits for the world to let go of it.
type localWatch struct {
	net.Conn
	cancel context.CancelFunc
	served <-chan struct{}
	once   sync.Once
	err    error
}

func (w *localWatch) Close() error {
	w.once.Do(func() {
		w.err = w.Conn.Close()
		w.cancel()
		<-w.served
	})
	return w.err
}

// localAddr is the remote of an in-process stream; the rate limiter keys on
// it, so local callers share one bucket, as one broker pod does over QUIC.
type localAddr struct{}

func (localAddr) Network() string { return "local" }
func (localAddr) String() string  { return "local" }
