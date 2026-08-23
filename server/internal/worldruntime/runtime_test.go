package worldruntime

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/filestore"
	"github.com/latebit-io/demarkus/server/internal/quicserve"
	servertls "github.com/latebit-io/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

func TestRuntimeServesHealthAndClosesBackendOnce(t *testing.T) {
	var closeCalls int
	runtime := newTestRuntime(t, &Config{
		CloseBackend: func() error {
			closeCalls++
			return nil
		},
	})
	stream := newTestStream("FETCH /health\n")
	runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), stream)

	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want %q", response.Status, protocol.StatusOK)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("backend close calls = %d, want 1", closeCalls)
	}
}

func TestRuntimeRejectsStreamsAfterClose(t *testing.T) {
	runtime := newTestRuntime(t, &Config{})
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stream := newTestStream("FETCH /health\n")
	runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), stream)
	if !stream.isClosed() {
		t.Fatal("stream accepted after runtime close")
	}
}

func TestRuntimeReloadKeepsLastValidTokens(t *testing.T) {
	tokensFile := t.TempDir() + "/tokens.toml"
	writeFile(t, tokensFile, tokenConfig("first"))
	runtime := newTestRuntime(t, &Config{TokensFile: tokensFile})
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	first := runtime.Tokens()

	writeFile(t, tokensFile, "invalid = [")
	if err := runtime.ReloadTokens(); err == nil {
		t.Fatal("ReloadTokens accepted malformed file")
	}
	if runtime.Tokens() != first {
		t.Fatal("malformed reload replaced current tokens")
	}

	writeFile(t, tokensFile, tokenConfig("second"))
	if err := runtime.ReloadTokens(); err != nil {
		t.Fatalf("ReloadTokens: %v", err)
	}
	wantHash := protocol.HashToken("second")
	if got := runtime.Tokens().Hashes(); len(got) != 1 || got[0] != wantHash {
		t.Fatalf("token hashes = %v, want [%s]", got, wantHash)
	}
}

func TestRuntimesIsolateTokensAndDocuments(t *testing.T) {
	firstTokens := t.TempDir() + "/tokens.toml"
	secondTokens := t.TempDir() + "/tokens.toml"
	writeFile(t, firstTokens, tokenConfig("first"))
	writeFile(t, secondTokens, tokenConfig("second"))
	first := newTestRuntime(t, &Config{TokensFile: firstTokens})
	second := newTestRuntime(t, &Config{TokensFile: secondTokens})
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Errorf("close first runtime: %v", err)
		}
		if err := second.Close(); err != nil {
			t.Errorf("close second runtime: %v", err)
		}
	})

	publish := "PUBLISH /doc.md\n---\nauth: first\n---\n# First\n"
	if status := serveStatus(t, first, publish); status != protocol.StatusCreated {
		t.Fatalf("first publish status = %q, want %q", status, protocol.StatusCreated)
	}
	if status := serveStatus(t, second, publish); status != protocol.StatusUnauthorized {
		t.Fatalf("cross-world publish status = %q, want %q", status, protocol.StatusUnauthorized)
	}
	if status := serveStatus(t, second, "FETCH /doc.md\n"); status != protocol.StatusNotFound {
		t.Fatalf("cross-world fetch status = %q, want %q", status, protocol.StatusNotFound)
	}
}

func TestRuntimeCloseDrainsActiveStream(t *testing.T) {
	runtime := newTestRuntime(t, &Config{})
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	stream := &testStream{Reader: reader}
	served := make(chan struct{})
	go func() {
		runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), stream)
		close(served)
	}()
	<-reader.started

	closed := make(chan error, 1)
	go func() { closed <- runtime.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before stream drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(reader.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after stream drained")
	}
	<-served
}

func TestRuntimeLimitsConcurrentStreams(t *testing.T) {
	runtime := newTestRuntime(t, &Config{MaxConcurrent: 1, RequestTimeout: 10 * time.Millisecond})
	reader := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	first := &testStream{Reader: reader}
	served := make(chan struct{})
	go func() {
		runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), first)
		close(served)
	}()
	<-reader.started

	second := newTestStream("FETCH /health\n")
	runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), second)
	response, err := protocol.ParseResponse(&second.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusRateLimited {
		t.Fatalf("status = %q, want %q", response.Status, protocol.StatusRateLimited)
	}
	close(reader.release)
	<-served
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRuntimeOverQUIC(t *testing.T) {
	runtime := newTestRuntime(t, &Config{})
	serverTLS, err := servertls.GenerateDevConfig()
	if err != nil {
		t.Fatalf("GenerateDevConfig: %v", err)
	}
	server, err := quicserve.Listen(quicserve.Config{
		Address:   "127.0.0.1:0",
		TLSConfig: serverTLS,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("server Close: %v", err)
		}
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime Close: %v", err)
		}
	})
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(context.Background(), func(*quic.Conn) (quicserve.Endpoint, error) {
			return runtime, nil
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTLS := &tls.Config{
		InsecureSkipVerify: true, // Test certificate is self-signed.
		NextProtos:         []string{protocol.ALPN},
	}
	connection, err := quic.DialAddr(ctx, server.Addr().String(), clientTLS, nil)
	if err != nil {
		t.Fatalf("DialAddr: %v", err)
	}
	stream, err := connection.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("OpenStreamSync: %v", err)
	}
	if _, err := io.WriteString(stream, "FETCH /health\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close request: %v", err)
	}
	response, err := protocol.ParseResponse(stream)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if response.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want %q", response.Status, protocol.StatusOK)
	}
	if err := connection.CloseWithError(0, "test complete"); err != nil {
		t.Fatalf("close connection: %v", err)
	}
	if err := server.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-serveResult; !errors.Is(err, quicserve.ErrServerClosed) {
		t.Fatalf("Serve error = %v, want ErrServerClosed", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime Close: %v", err)
	}
}

func TestWriteRateLimited(t *testing.T) {
	stream := newTestStream("")
	if err := writeRateLimited(stream); err != nil {
		t.Fatalf("writeRateLimited: %v", err)
	}
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if response.Status != protocol.StatusRateLimited {
		t.Fatalf("status = %q, want %q", response.Status, protocol.StatusRateLimited)
	}
}

func newTestRuntime(t *testing.T, config *Config) *Runtime {
	t.Helper()
	documents, err := protocolstore.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	lookup := catalog.New()
	store := filestore.New(documents, lookup)
	config.Store = store
	config.Catalog = store
	config.Views = store
	config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	runtime, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runtime
}

func tokenConfig(hash string) string {
	return "[tokens.test]\n" +
		"hash = \"" + protocol.HashToken(hash) + "\"\n" +
		"paths = [\"/*\"]\n" +
		"operations = [\"publish\"]\n"
}

func serveStatus(t *testing.T, runtime *Runtime, request string) string {
	t.Helper()
	stream := newTestStream(request)
	runtime.ServeStream(context.Background(), testAddr("127.0.0.1:1234"), stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return response.Status
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

type testStream struct {
	io.Reader
	output bytes.Buffer
	mu     sync.Mutex
	closed bool
}

func newTestStream(request string) *testStream {
	return &testStream{Reader: strings.NewReader(request)}
}

func (s *testStream) Write(p []byte) (int, error)     { return s.output.Write(p) }
func (s *testStream) SetReadDeadline(time.Time) error { return nil }
func (s *testStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *testStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

var _ net.Addr = testAddr("")

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}
