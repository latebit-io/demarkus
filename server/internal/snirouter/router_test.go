package snirouter

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/quicserve"
	"github.com/quic-go/quic-go"
)

func TestNormalizeAuthority(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		want      string
		wantErr   bool
	}{
		{name: "canonical", authority: "world.example", want: "world.example"},
		{name: "ASCII case folded", authority: "WoRlD.Example", want: "world.example"},
		{name: "punycode A-label", authority: "xn--bcher-kva.example", want: "xn--bcher-kva.example"},
		{name: "empty", authority: "", wantErr: true},
		{name: "wildcard", authority: "*.example", wantErr: true},
		{name: "trailing dot", authority: "world.example.", wantErr: true},
		{name: "port", authority: "world.example:6309", wantErr: true},
		{name: "Unicode", authority: "b\u00fccher.example", wantErr: true},
		{name: "IPv4 literal", authority: "192.0.2.1", wantErr: true},
		{name: "IPv6 literal", authority: "2001:db8::1", wantErr: true},
		{name: "bracketed IPv6 literal", authority: "[2001:db8::1]", wantErr: true},
		{name: "empty label", authority: "world..example", wantErr: true},
		{name: "leading hyphen", authority: "-world.example", wantErr: true},
		{name: "trailing hyphen", authority: "world-.example", wantErr: true},
		{name: "underscore", authority: "world_name.example", wantErr: true},
		{name: "label too long", authority: strings.Repeat("a", 64) + ".example", wantErr: true},
		{name: "name too long", authority: longDNSName(), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeAuthority(test.authority)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidAuthority) {
					t.Fatalf("NormalizeAuthority() error = %v, want ErrInvalidAuthority", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeAuthority() error: %v", err)
			}
			if got != test.want {
				t.Fatalf("NormalizeAuthority() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Run("rejects duplicate normalized authorities", func(t *testing.T) {
		_, err := New([]Mapping{
			{Authority: "World.Example", Endpoint: testEndpoint("first")},
			{Authority: "world.example", Endpoint: testEndpoint("second")},
		})
		if !errors.Is(err, ErrDuplicateAuthority) {
			t.Fatalf("New() error = %v, want ErrDuplicateAuthority", err)
		}
	})

	t.Run("rejects invalid configured authority", func(t *testing.T) {
		_, err := New([]Mapping{{Authority: "*.example", Endpoint: testEndpoint("world")}})
		if !errors.Is(err, ErrInvalidAuthority) {
			t.Fatalf("New() error = %v, want ErrInvalidAuthority", err)
		}
	})

	t.Run("rejects nil endpoint", func(t *testing.T) {
		_, err := New([]Mapping{{Authority: "world.example"}})
		if err == nil {
			t.Fatal("New() error = nil, want error")
		}
	})

	t.Run("copies mappings", func(t *testing.T) {
		first := testEndpoint("first")
		mappings := []Mapping{{Authority: "world.example", Endpoint: first}}
		router, err := New(mappings)
		if err != nil {
			t.Fatalf("New() error: %v", err)
		}
		mappings[0] = Mapping{Authority: "other.example", Endpoint: testEndpoint("other")}

		got, err := router.lookup("world.example")
		if err != nil {
			t.Fatalf("lookup() error: %v", err)
		}
		if got != first {
			t.Fatalf("lookup() = %v, want original endpoint", got)
		}
		if _, err := router.lookup("other.example"); !errors.Is(err, ErrUnknownAuthority) {
			t.Fatalf("lookup(other.example) error = %v, want ErrUnknownAuthority", err)
		}
	})
}

func TestHandshakeHook(t *testing.T) {
	endpoint := testEndpoint("world")
	router := mustRouter(t, []Mapping{{Authority: "world.example", Endpoint: endpoint}})

	t.Run("case folds known SNI and preserves TLS config", func(t *testing.T) {
		var certificateCalls atomic.Int32
		config := &tls.Config{ //nolint:gosec // Test config is not used on a network.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				certificateCalls.Add(1)
				return nil, nil
			},
		}
		hook, err := router.HandshakeHook(config)
		if err != nil {
			t.Fatalf("HandshakeHook() error: %v", err)
		}

		got, err := hook(&tls.ClientHelloInfo{ServerName: "WORLD.Example"})
		if err != nil {
			t.Fatalf("hook() error: %v", err)
		}
		if got != config {
			t.Fatalf("hook() config = %p, want supplied config %p", got, config)
		}
		if _, err := got.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
			t.Fatalf("GetCertificate() error: %v", err)
		}
		if certificateCalls.Load() != 1 {
			t.Fatalf("GetCertificate() calls = %d, want 1", certificateCalls.Load())
		}
	})

	t.Run("delegates existing config hook", func(t *testing.T) {
		selected := &tls.Config{MinVersion: tls.VersionTLS13}
		config := &tls.Config{ //nolint:gosec // Test config is not used on a network.
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				return selected, nil
			},
		}
		hook, err := router.HandshakeHook(config)
		if err != nil {
			t.Fatalf("HandshakeHook() error: %v", err)
		}
		got, err := hook(&tls.ClientHelloInfo{ServerName: "world.example"})
		if err != nil {
			t.Fatalf("hook() error: %v", err)
		}
		if got != selected {
			t.Fatalf("hook() config = %p, want delegated config %p", got, selected)
		}
	})

	t.Run("rejects absent unknown and malformed SNI", func(t *testing.T) {
		hook, err := router.HandshakeHook(&tls.Config{MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatalf("HandshakeHook() error: %v", err)
		}
		tests := []struct {
			name       string
			serverName string
			wantErr    error
		}{
			{name: "absent", wantErr: ErrInvalidAuthority},
			{name: "unknown", serverName: "unknown.example", wantErr: ErrUnknownAuthority},
			{name: "wildcard", serverName: "*.example", wantErr: ErrInvalidAuthority},
			{name: "trailing dot", serverName: "world.example.", wantErr: ErrInvalidAuthority},
			{name: "port", serverName: "world.example:6309", wantErr: ErrInvalidAuthority},
			{name: "Unicode", serverName: "b\u00fccher.example", wantErr: ErrInvalidAuthority},
			{name: "IP literal", serverName: "192.0.2.1", wantErr: ErrInvalidAuthority},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got, err := hook(&tls.ClientHelloInfo{ServerName: test.serverName})
				if got != nil {
					t.Fatalf("hook() config = %p, want nil", got)
				}
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("hook() error = %v, want %v", err, test.wantErr)
				}
			})
		}
	})

	t.Run("has no default", func(t *testing.T) {
		emptyRouter := mustRouter(t, nil)
		hook, err := emptyRouter.HandshakeHook(&tls.Config{MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatalf("HandshakeHook() error: %v", err)
		}
		if _, err := hook(&tls.ClientHelloInfo{ServerName: "world.example"}); !errors.Is(err, ErrUnknownAuthority) {
			t.Fatalf("hook() error = %v, want ErrUnknownAuthority", err)
		}
	})
}

func TestPostHandshakeSelection(t *testing.T) {
	t.Run("selects negotiated SNI and repeats exact lookup", func(t *testing.T) {
		endpoint := testEndpoint("world")
		router := mustRouter(t, []Mapping{{Authority: "world.example", Endpoint: endpoint}})
		certificate, roots := testCertificate(t, "world.example")
		var certificateCalls atomic.Int32
		serverTLS := &tls.Config{
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{"snirouter-test"},
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				certificateCalls.Add(1)
				return &certificate, nil
			},
		}
		hook, err := router.HandshakeHook(serverTLS)
		if err != nil {
			t.Fatalf("HandshakeHook() error: %v", err)
		}
		serverTLS.GetConfigForClient = hook

		listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, nil)
		if err != nil {
			t.Fatalf("quic.ListenAddr() error: %v", err)
		}
		t.Cleanup(func() {
			if err := listener.Close(); err != nil {
				t.Errorf("listener.Close() error: %v", err)
			}
		})

		acceptResult := make(chan connectionResult, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		go func() {
			connection, acceptErr := listener.Accept(ctx)
			acceptResult <- connectionResult{connection: connection, err: acceptErr}
		}()

		clientTLS := &tls.Config{
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{"snirouter-test"},
			RootCAs:    roots,
			ServerName: "WORLD.Example",
		}
		clientConnection, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, nil)
		if err != nil {
			t.Fatalf("quic.DialAddr() error: %v", err)
		}
		t.Cleanup(func() {
			if err := clientConnection.CloseWithError(0, "test complete"); err != nil {
				t.Errorf("client CloseWithError() error: %v", err)
			}
		})

		accepted := <-acceptResult
		if accepted.err != nil {
			t.Fatalf("listener.Accept() error: %v", accepted.err)
		}
		t.Cleanup(func() {
			if err := accepted.connection.CloseWithError(0, "test complete"); err != nil {
				t.Errorf("server CloseWithError() error: %v", err)
			}
		})

		selector := router.Selector()
		got, err := selector(accepted.connection)
		if err != nil {
			t.Fatalf("Selector() error: %v", err)
		}
		if got != endpoint {
			t.Fatalf("Selector() = %v, want configured endpoint", got)
		}
		if certificateCalls.Load() != 1 {
			t.Fatalf("GetCertificate() calls = %d, want 1", certificateCalls.Load())
		}

		otherRouter := mustRouter(t, []Mapping{{Authority: "other.example", Endpoint: testEndpoint("other")}})
		if _, err := otherRouter.Select(accepted.connection); !errors.Is(err, ErrUnknownAuthority) {
			t.Fatalf("defensive Select() error = %v, want ErrUnknownAuthority", err)
		}
	})

	t.Run("rejects nil connection", func(t *testing.T) {
		router := mustRouter(t, nil)
		if _, err := router.Select(nil); err == nil {
			t.Fatal("Select(nil) error = nil, want error")
		}
	})
}

type testEndpoint string

func (testEndpoint) ServeStream(context.Context, net.Addr, quicserve.Stream) {}

type connectionResult struct {
	connection *quic.Conn
	err        error
}

func mustRouter(t *testing.T, mappings []Mapping) *Router {
	t.Helper()
	router, err := New(mappings)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return router
}

func longDNSName() string {
	label := strings.Repeat("a", 63)
	return strings.Join([]string{label, label, label, label}, ".")
}

func testCertificate(t *testing.T, serverName string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate() error: %v", err)
	}
	parsed, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatalf("x509.ParseCertificate() error: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{
		Certificate: [][]byte{certificateDER},
		PrivateKey:  privateKey,
		Leaf:        parsed,
	}, roots
}
