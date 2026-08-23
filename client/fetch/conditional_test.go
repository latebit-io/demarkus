package fetch

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

// startTestServer runs a minimal in-process QUIC server whose handler maps a
// parsed request to a response. Returns the host:port to dial.
func startTestServer(t *testing.T, handle func(protocol.Request) protocol.Response) string {
	return startTestServerWithSNI(t, nil, handle)
}

func startTestServerWithSNI(t *testing.T, observed chan<- string, handle func(protocol.Request) protocol.Response) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{protocol.ALPN},
	}
	if observed != nil {
		tlsConf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			observed <- hello.ServerName
			return nil, nil
		}
	}

	ln, err := quic.ListenAddr("127.0.0.1:0", tlsConf, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			go func() {
				for {
					stream, err := conn.AcceptStream(context.Background())
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = stream.Close() }()
						req, err := protocol.ParseRequest(stream)
						if err != nil {
							return
						}
						resp := handle(req)
						// Ignore write errors; the client side of the test
						// fails on the missing response.
						_, _ = resp.WriteTo(stream)
					}()
				}
			}()
		}
	}()

	return ln.Addr().String()
}

func TestEndpointOverridesKeepLogicalConnectionsSeparate(t *testing.T) {
	observed := make(chan string, 2)
	dialAddress := startTestServerWithSNI(t, observed, func(protocol.Request) protocol.Response {
		return protocol.Response{Status: protocol.StatusOK, Body: "# OK\n"}
	})

	c := NewClient(Options{Insecure: true, Endpoints: map[string]Endpoint{
		"world-a:6309": {DialAddress: dialAddress, ServerName: "world-a.internal"},
		"world-b:6309": {DialAddress: dialAddress, ServerName: "world-b.internal"},
	}})
	defer c.Close()

	for _, authority := range []string{"world-a:6309", "world-b:6309"} {
		result, err := c.Fetch(authority, "/index.md", "")
		if err != nil {
			t.Fatalf("Fetch(%s): %v", authority, err)
		}
		if result.Response.Status != protocol.StatusOK {
			t.Fatalf("Fetch(%s) status = %q", authority, result.Response.Status)
		}
	}

	got := map[string]bool{<-observed: true, <-observed: true}
	for _, want := range []string{"world-a.internal", "world-b.internal"} {
		if !got[want] {
			t.Errorf("observed SNI = %v, missing %q", got, want)
		}
	}
}

func TestParseMarkURLNormalizesAuthority(t *testing.T) {
	tests := []struct {
		raw      string
		wantHost string
		wantPath string
	}{
		{"mark://WORLD/Index.md", "world:6309", "/Index.md"},
		{"mark://world:7000", "world:7000", "/"},
		{"mark://[2001:db8::1]/index.md", "[2001:db8::1]:6309", "/index.md"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			host, path, err := ParseMarkURL(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if host != tt.wantHost || path != tt.wantPath {
				t.Errorf("ParseMarkURL(%q) = (%q, %q), want (%q, %q)", tt.raw, host, path, tt.wantHost, tt.wantPath)
			}
		})
	}

	for _, raw := range []string{"mark://", "mark://world:0", "mark://world:65536"} {
		t.Run("invalid "+raw, func(t *testing.T) {
			if _, _, err := ParseMarkURL(raw); err == nil {
				t.Fatalf("ParseMarkURL(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestFetchConditional(t *testing.T) {
	const etag = "etag-1"
	host := startTestServer(t, func(req protocol.Request) protocol.Response {
		if req.Verb != protocol.VerbFetch {
			return protocol.Response{Status: protocol.StatusBadRequest}
		}
		if req.Metadata["if-none-match"] == etag {
			return protocol.Response{Status: protocol.StatusNotModified, Metadata: map[string]string{"etag": etag}}
		}
		return protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"etag": etag}, Body: "# Graph\n"}
	})

	c := NewClient(Options{Insecure: true})
	defer c.Close()

	// No etag: plain fetch, full body.
	r, err := c.FetchConditional(host, "/graph.md", "", "")
	if err != nil {
		t.Fatalf("FetchConditional without etag: %v", err)
	}
	if r.Response.Status != protocol.StatusOK || r.Response.Body != "# Graph\n" {
		t.Fatalf("status = %q body = %q, want ok with body", r.Response.Status, r.Response.Body)
	}
	if r.Response.Metadata["etag"] != etag {
		t.Fatalf("etag = %q, want %q", r.Response.Metadata["etag"], etag)
	}

	// Matching etag: not-modified passthrough with empty body.
	r, err = c.FetchConditional(host, "/graph.md", "", etag)
	if err != nil {
		t.Fatalf("FetchConditional with etag: %v", err)
	}
	if r.Response.Status != protocol.StatusNotModified {
		t.Fatalf("status = %q, want not-modified", r.Response.Status)
	}
	if r.Response.Body != "" {
		t.Fatalf("body = %q, want empty on not-modified", r.Response.Body)
	}

	// Stale etag: full body again.
	r, err = c.FetchConditional(host, "/graph.md", "", "stale")
	if err != nil {
		t.Fatalf("FetchConditional with stale etag: %v", err)
	}
	if r.Response.Status != protocol.StatusOK || r.Response.Body == "" {
		t.Fatalf("status = %q body = %q, want ok with body", r.Response.Status, r.Response.Body)
	}
}
