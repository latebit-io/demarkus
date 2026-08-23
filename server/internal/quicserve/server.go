// Package quicserve owns QUIC listener, connection, and stream lifecycles.
package quicserve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// ErrServerClosed is returned by Serve after Shutdown or Close stops the server.
var ErrServerClosed = errors.New("quicserve: server closed")

// Stream is a bidirectional QUIC stream.
type Stream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
}

// Endpoint handles streams for one accepted connection.
type Endpoint interface {
	ServeStream(context.Context, net.Addr, Stream)
}

// Selector chooses the endpoint pinned to an accepted connection.
type Selector func(*quic.Conn) (Endpoint, error)

// Config configures a QUIC server listener.
type Config struct {
	Address    string
	TLSConfig  *tls.Config
	QUICConfig *quic.Config
	Logger     *slog.Logger
}

// Server owns a QUIC listener and all connections accepted from it.
type Server struct {
	listener connectionListener
	logger   *slog.Logger

	acceptCtx    context.Context
	cancelAccept context.CancelFunc

	mu           sync.Mutex
	connections  map[*trackedConnection]struct{}
	serveStarted bool
	serveRunning bool
	stopping     bool
	closeErrors  []error
	drained      chan struct{}
	drainOnce    sync.Once
	serveDone    chan struct{}

	stopOnce sync.Once
	stopDone chan struct{}
	stopErr  error

	closeOnce sync.Once
	closeErr  error
}

// Listen opens a QUIC listener.
func Listen(config Config) (*Server, error) {
	if config.Address == "" {
		return nil, errors.New("quicserve: address is empty")
	}
	if config.TLSConfig == nil {
		return nil, errors.New("quicserve: TLS config is nil")
	}

	listener, err := quic.ListenAddr(config.Address, config.TLSConfig, config.QUICConfig)
	if err != nil {
		return nil, fmt.Errorf("quicserve: listen on %s: %w", config.Address, err)
	}
	return newServer(&quicListener{Listener: listener}, config.Logger), nil
}

func newServer(listener connectionListener, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	acceptCtx, cancelAccept := context.WithCancel(context.Background())
	return &Server{
		listener:     listener,
		logger:       logger,
		acceptCtx:    acceptCtx,
		cancelAccept: cancelAccept,
		connections:  make(map[*trackedConnection]struct{}),
		drained:      make(chan struct{}),
		serveDone:    make(chan struct{}),
		stopDone:     make(chan struct{}),
	}
}

// Addr returns the listener's network address.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Serve accepts connections until the context ends or the server is stopped.
func (s *Server) Serve(ctx context.Context, selector Selector) error {
	if selector == nil {
		return errors.New("quicserve: selector is nil")
	}
	if err := s.startServing(); err != nil {
		return err
	}
	defer s.finishServing()

	for {
		connection, err := s.listener.Accept(ctx)
		if err != nil {
			if s.isStopping() {
				return errors.Join(ErrServerClosed, s.stopError())
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return errors.Join(ctxErr, s.stop())
			}
			acceptErr := fmt.Errorf("quicserve: accept connection: %w", err)
			return errors.Join(acceptErr, s.stop())
		}

		handlerCtx, cancelHandler := context.WithCancel(context.WithoutCancel(ctx))
		tracked := &trackedConnection{
			connection: connection,
			server:     s,
			handlerCtx: handlerCtx,
			cancel:     cancelHandler,
		}
		if !s.track(tracked) {
			tracked.close()
			continue
		}
		go s.serveConnection(tracked, selector)
	}
}

func (s *Server) startServing() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return ErrServerClosed
	}
	if s.serveStarted {
		return errors.New("quicserve: Serve called more than once")
	}
	s.serveStarted = true
	s.serveRunning = true
	return nil
}

func (s *Server) finishServing() {
	s.mu.Lock()
	s.serveRunning = false
	s.maybeCloseDrained()
	s.mu.Unlock()
	close(s.serveDone)
}

func (s *Server) serveConnection(tracked *trackedConnection, selector Selector) {
	defer s.untrack(tracked)

	endpoint, err := selector(tracked.connection.QUICConn())
	if err != nil {
		s.logger.Warn("quicserve: selecting endpoint failed", "remote", tracked.connection.RemoteAddr(), "error", err)
		tracked.close()
		return
	}
	if endpoint == nil {
		s.logger.Warn("quicserve: selector returned nil endpoint", "remote", tracked.connection.RemoteAddr())
		tracked.close()
		return
	}

	for {
		stream, err := tracked.connection.AcceptStream(s.acceptCtx)
		if err != nil {
			if s.acceptCtx.Err() == nil {
				s.logger.Debug("quicserve: accepting stream ended", "remote", tracked.connection.RemoteAddr(), "error", err)
			}
			break
		}

		tracked.streams.Go(func() {
			endpoint.ServeStream(tracked.handlerCtx, tracked.connection.RemoteAddr(), stream)
		})
	}

	tracked.streams.Wait()
	tracked.close()
}

func (s *Server) track(connection *trackedConnection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return false
	}
	s.connections[connection] = struct{}{}
	return true
}

func (s *Server) untrack(connection *trackedConnection) {
	s.mu.Lock()
	delete(s.connections, connection)
	s.maybeCloseDrained()
	s.mu.Unlock()
}

func (s *Server) maybeCloseDrained() {
	if s.stopping && !s.serveRunning && len(s.connections) == 0 {
		s.drainOnce.Do(func() { close(s.drained) })
	}
}

func (s *Server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

func (s *Server) stop() error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopping = true
		s.maybeCloseDrained()
		s.mu.Unlock()

		s.cancelAccept()
		s.stopErr = s.listener.Close()
		close(s.stopDone)
	})
	<-s.stopDone
	return s.stopErr
}

func (s *Server) stopError() error {
	<-s.stopDone
	return s.stopErr
}

// Shutdown stops acceptance and waits for accepted stream handlers to drain.
func (s *Server) Shutdown(ctx context.Context) error {
	stopErr := s.stop()
	select {
	case <-s.drained:
		return errors.Join(stopErr, s.connectionCloseError())
	case <-ctx.Done():
		s.forceClose()
		return errors.Join(stopErr, s.connectionCloseError(), ctx.Err())
	}
}

// Close immediately closes the listener and every accepted connection.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		stopErr := s.stop()
		s.waitForServe()
		s.forceClose()
		s.closeErr = errors.Join(stopErr, s.connectionCloseError())
	})
	return s.closeErr
}

func (s *Server) waitForServe() {
	s.mu.Lock()
	serveStarted := s.serveStarted
	s.mu.Unlock()
	if serveStarted {
		<-s.serveDone
	}
}

func (s *Server) forceClose() {
	s.mu.Lock()
	connections := make([]*trackedConnection, 0, len(s.connections))
	for connection := range s.connections {
		connections = append(connections, connection)
	}
	s.mu.Unlock()

	for _, connection := range connections {
		connection.close()
	}
}

func (s *Server) recordCloseError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.closeErrors = append(s.closeErrors, err)
	s.mu.Unlock()
}

func (s *Server) connectionCloseError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(s.closeErrors...)
}

type connectionListener interface {
	Accept(context.Context) (acceptedConnection, error)
	Addr() net.Addr
	Close() error
}

type acceptedConnection interface {
	QUICConn() *quic.Conn
	AcceptStream(context.Context) (Stream, error)
	RemoteAddr() net.Addr
	CloseWithError(quic.ApplicationErrorCode, string) error
}

type quicListener struct {
	*quic.Listener
}

func (l *quicListener) Accept(ctx context.Context) (acceptedConnection, error) {
	connection, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &quicConnection{Conn: connection}, nil
}

type quicConnection struct {
	*quic.Conn
}

func (c *quicConnection) QUICConn() *quic.Conn {
	return c.Conn
}

func (c *quicConnection) AcceptStream(ctx context.Context) (Stream, error) {
	return c.Conn.AcceptStream(ctx)
}

type trackedConnection struct {
	connection acceptedConnection
	server     *Server
	handlerCtx context.Context
	cancel     context.CancelFunc
	streams    sync.WaitGroup
	closeOnce  sync.Once
}

func (c *trackedConnection) close() {
	c.closeOnce.Do(func() {
		c.cancel()
		c.server.recordCloseError(c.connection.CloseWithError(0, "server closed"))
	})
}
