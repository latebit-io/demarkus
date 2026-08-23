package quicserve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestSelectorPinnedToConnection(t *testing.T) {
	t.Run("selector runs once and handles every stream", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)

		var selectorCalls atomic.Int32
		served := make(chan Stream, 2)
		endpoint := endpointFunc(func(_ context.Context, _ net.Addr, stream Stream) {
			served <- stream
		})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			selectorCalls.Add(1)
			return endpoint, nil
		})

		first := newFakeStream()
		second := newFakeStream()
		connection.accept(first)
		connection.accept(second)
		got := map[Stream]bool{
			waitForStream(t, served): true,
			waitForStream(t, served): true,
		}
		if !got[first] || !got[second] {
			t.Fatalf("served streams = %v, want both accepted streams", got)
		}

		if selectorCalls.Load() != 1 {
			t.Fatalf("selector calls = %d, want 1", selectorCalls.Load())
		}
		shutdownServer(t, server, serveResult)
	})
}

func TestSelectorFailure(t *testing.T) {
	t.Run("failed selection closes connection", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)

		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			return nil, errors.New("no endpoint")
		})
		connection.waitClosed(t)
		if connection.closeCalls.Load() != 1 {
			t.Fatalf("close calls = %d, want 1", connection.closeCalls.Load())
		}
		shutdownServer(t, server, serveResult)
	})
}

func TestServeListenerFailure(t *testing.T) {
	t.Run("unexpected listener error is returned", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		acceptErr := errors.New("listener failed")
		listener.fail(acceptErr)

		err := <-runServe(server, func(*quic.Conn) (Endpoint, error) {
			return endpointFunc(func(context.Context, net.Addr, Stream) {}), nil
		})
		if !errors.Is(err, acceptErr) {
			t.Fatalf("Serve error = %v, want %v", err, acceptErr)
		}
		if errors.Is(err, ErrServerClosed) {
			t.Fatalf("Serve error = %v, unexpectedly ErrServerClosed", err)
		}
	})
}

func TestShutdown(t *testing.T) {
	t.Run("graceful shutdown drains active handler", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)
		started := make(chan struct{})
		release := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			return endpointFunc(func(context.Context, net.Addr, Stream) {
				close(started)
				<-release
			}), nil
		})
		connection.accept(newFakeStream())
		waitClosed(t, started, "handler start")

		shutdownResult := make(chan error, 1)
		go func() { shutdownResult <- server.Shutdown(context.Background()) }()
		assertNotClosed(t, shutdownResult, "Shutdown returned before handler drained")
		close(release)
		if err := waitError(t, shutdownResult, "Shutdown"); err != nil {
			t.Fatalf("Shutdown error: %v", err)
		}
		connection.waitClosed(t)
		assertServerClosed(t, serveResult)
	})

	t.Run("idle connection closes during shutdown", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)
		selected := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			close(selected)
			return endpointFunc(func(context.Context, net.Addr, Stream) {}), nil
		})
		waitClosed(t, selected, "selection")

		if err := server.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown error: %v", err)
		}
		connection.waitClosed(t)
		assertServerClosed(t, serveResult)
	})

	t.Run("deadline force closes active connection", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)
		started := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			return endpointFunc(func(_ context.Context, _ net.Addr, stream Stream) {
				close(started)
				buffer := make([]byte, 1)
				_, _ = stream.Read(buffer)
			}), nil
		})
		stream := newFakeStream()
		connection.accept(stream)
		waitClosed(t, started, "handler start")

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := server.Shutdown(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
		}
		connection.waitClosed(t)
		waitClosed(t, server.drained, "server drain")
		assertServerClosed(t, serveResult)
	})

	t.Run("close errors are joined", func(t *testing.T) {
		listener := newFakeListener()
		listener.closeErr = errors.New("listener close")
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		connection.closeErr = errors.New("connection close")
		listener.accept(connection)
		selected := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			close(selected)
			return endpointFunc(func(context.Context, net.Addr, Stream) {}), nil
		})
		waitClosed(t, selected, "selection")

		err := server.Shutdown(context.Background())
		if !errors.Is(err, listener.closeErr) || !errors.Is(err, connection.closeErr) {
			t.Fatalf("Shutdown error = %v, want joined close errors", err)
		}
		assertServerClosed(t, serveResult)
	})
}

func TestClose(t *testing.T) {
	t.Run("close is immediate and idempotent", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)
		selected := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			close(selected)
			return endpointFunc(func(context.Context, net.Addr, Stream) {}), nil
		})
		waitClosed(t, selected, "selection")

		if err := server.Close(); err != nil {
			t.Fatalf("first Close error: %v", err)
		}
		if err := server.Close(); err != nil {
			t.Fatalf("second Close error: %v", err)
		}
		connection.waitClosed(t)
		if listener.closeCalls.Load() != 1 {
			t.Fatalf("listener close calls = %d, want 1", listener.closeCalls.Load())
		}
		if connection.closeCalls.Load() != 1 {
			t.Fatalf("connection close calls = %d, want 1", connection.closeCalls.Load())
		}
		waitClosed(t, server.drained, "server drain")
		assertServerClosed(t, serveResult)
	})

	t.Run("close errors are joined", func(t *testing.T) {
		listener := newFakeListener()
		listener.closeErr = errors.New("listener close")
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		connection.closeErr = errors.New("connection close")
		listener.accept(connection)
		selected := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			close(selected)
			return endpointFunc(func(context.Context, net.Addr, Stream) {}), nil
		})
		waitClosed(t, selected, "selection")

		err := server.Close()
		if !errors.Is(err, listener.closeErr) || !errors.Is(err, connection.closeErr) {
			t.Fatalf("Close error = %v, want joined close errors", err)
		}
		assertServerClosed(t, serveResult)
	})
}

func TestServerGoroutinesExit(t *testing.T) {
	t.Run("force close releases stream and connection loops", func(t *testing.T) {
		listener := newFakeListener()
		server := newServer(listener, discardLogger())
		connection := newFakeConnection()
		listener.accept(connection)
		started := make(chan struct{})
		serveResult := runServe(server, func(*quic.Conn) (Endpoint, error) {
			return endpointFunc(func(_ context.Context, _ net.Addr, stream Stream) {
				close(started)
				buffer := make([]byte, 1)
				_, _ = stream.Read(buffer)
			}), nil
		})
		connection.accept(newFakeStream())
		waitClosed(t, started, "handler start")

		if err := server.Close(); err != nil {
			t.Fatalf("Close error: %v", err)
		}
		assertServerClosed(t, serveResult)
		waitClosed(t, server.drained, "server drain")
	})
}

type endpointFunc func(context.Context, net.Addr, Stream)

func (f endpointFunc) ServeStream(ctx context.Context, addr net.Addr, stream Stream) {
	f(ctx, addr, stream)
}

type acceptResult struct {
	connection acceptedConnection
	err        error
}

type fakeListener struct {
	results    chan acceptResult
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls atomic.Int32
	closeErr   error
}

func newFakeListener() *fakeListener {
	return &fakeListener{
		results: make(chan acceptResult, 8),
		closed:  make(chan struct{}),
	}
}

func (l *fakeListener) Accept(ctx context.Context) (acceptedConnection, error) {
	select {
	case result := <-l.results:
		return result.connection, result.err
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *fakeListener) Addr() net.Addr {
	return fakeAddr("listener")
}

func (l *fakeListener) Close() error {
	l.closeOnce.Do(func() {
		l.closeCalls.Add(1)
		close(l.closed)
	})
	return l.closeErr
}

func (l *fakeListener) accept(connection acceptedConnection) {
	l.results <- acceptResult{connection: connection}
}

func (l *fakeListener) fail(err error) {
	l.results <- acceptResult{err: err}
}

type fakeConnection struct {
	streams    chan acceptStreamResult
	closed     chan struct{}
	closeOnce  sync.Once
	closeCalls atomic.Int32
	closeErr   error
	mu         sync.Mutex
	accepted   []*fakeStream
}

type acceptStreamResult struct {
	stream Stream
	err    error
}

func newFakeConnection() *fakeConnection {
	return &fakeConnection{
		streams: make(chan acceptStreamResult, 8),
		closed:  make(chan struct{}),
	}
}

func (c *fakeConnection) QUICConn() *quic.Conn {
	return nil
}

func (c *fakeConnection) AcceptStream(ctx context.Context) (Stream, error) {
	select {
	case result := <-c.streams:
		return result.stream, result.err
	case <-c.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeConnection) RemoteAddr() net.Addr {
	return fakeAddr("remote")
}

func (c *fakeConnection) CloseWithError(quic.ApplicationErrorCode, string) error {
	c.closeOnce.Do(func() {
		c.closeCalls.Add(1)
		close(c.closed)
		c.mu.Lock()
		streams := append([]*fakeStream(nil), c.accepted...)
		c.mu.Unlock()
		for _, stream := range streams {
			// fakeStream.Close only closes its done channel.
			_ = stream.Close()
		}
	})
	return c.closeErr
}

func (c *fakeConnection) accept(stream *fakeStream) {
	c.mu.Lock()
	c.accepted = append(c.accepted, stream)
	c.mu.Unlock()
	c.streams <- acceptStreamResult{stream: stream}
}

func (c *fakeConnection) waitClosed(t *testing.T) {
	t.Helper()
	waitClosed(t, c.closed, "connection close")
}

type fakeStream struct {
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{closed: make(chan struct{})}
}

func (s *fakeStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *fakeStream) Write(p []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, io.ErrClosedPipe
	default:
		return len(p), nil
	}
}

func (s *fakeStream) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

func (s *fakeStream) SetReadDeadline(time.Time) error {
	return nil
}

type fakeAddr string

func (a fakeAddr) Network() string { return "fake" }
func (a fakeAddr) String() string  { return string(a) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func runServe(server *Server, selector Selector) <-chan error {
	result := make(chan error, 1)
	go func() { result <- server.Serve(context.Background(), selector) }()
	return result
}

func shutdownServer(t *testing.T, server *Server, serveResult <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}
	assertServerClosed(t, serveResult)
}

func assertServerClosed(t *testing.T, result <-chan error) {
	t.Helper()
	err := waitError(t, result, "Serve")
	if !errors.Is(err, ErrServerClosed) {
		t.Fatalf("Serve error = %v, want ErrServerClosed", err)
	}
}

func waitForStream(t *testing.T, served <-chan Stream) Stream {
	t.Helper()
	select {
	case got := <-served:
		return got
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream")
		return nil
	}
}

func waitClosed(t *testing.T, channel <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitError(t *testing.T, channel <-chan error, operation string) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}

func assertNotClosed(t *testing.T, channel <-chan error, message string) {
	t.Helper()
	select {
	case err := <-channel:
		t.Fatalf("%s: %v", message, err)
	case <-time.After(20 * time.Millisecond):
	}
}
