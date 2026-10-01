package knowledgeserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

const (
	bearerAuthority = "alice.memory.svc.cluster.local"
	otherAuthority  = "bob.memory.svc.cluster.local"
)

// scriptedGates admits alice's world only. Bearers: writer may write
// /notes/**, reader and cap read, stranger is not permitted, the rest are
// unauthorized. cap is also a read capability token in the world's store.
type scriptedGates struct {
	expires time.Time
}

func (s *scriptedGates) gates(authority string) (protocol.Gate, error) {
	if authority != bearerAuthority {
		return nil, fmt.Errorf("no gate for %s", authority)
	}
	return func(_ context.Context, req protocol.Request) (protocol.Grant, error) {
		switch req.Metadata["auth"] {
		case "writer":
			return protocol.Grant{Label: "alice@example.com", Paths: []string{"/notes/**"}, Expires: s.expires}, nil
		case "reader", "cap":
			return protocol.Grant{Label: "eve@example.com", Expires: s.expires}, nil
		case "stranger":
			return protocol.Grant{}, protocol.ErrNotPermitted
		default:
			return protocol.Grant{}, errors.New("signature invalid")
		}
	}, nil
}

// openBearerListener serves alice's and bob's worlds on a bearer listener
// at a loopback port and returns its address.
func openBearerListener(t *testing.T, scripted *scriptedGates) string {
	t.Helper()
	dir := t.TempDir()
	aliceTokens := filepath.Join(dir, "alice.toml")
	capEntry := "[tokens.cap]\nhash = \"" + protocol.HashToken("cap") + "\"\npaths = [\"/private/**\"]\noperations = [\"read\"]\n"
	if err := os.WriteFile(aliceTokens, []byte(capEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	bobTokens := writeTokens(t, dir, "bob")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, aliceTokens, true)+worldFragment("bob", testWorldIDB, bobTokens, true))
	server := &Server{config: h.config, certificates: h.certs, worlds: h.manager, logger: slog.New(slog.DiscardHandler)}
	if err := server.OpenBearerListener("127.0.0.1:0", scripted.gates); err != nil {
		t.Fatalf("OpenBearerListener: %v", err)
	}
	// The test server opens no 6309 listener, so the bearer listener is the only one.
	bearer := server.listeners[0]
	served := make(chan error, 1)
	go func() { served <- bearer.serve() }()
	t.Cleanup(func() {
		if err := bearer.server.Close(); err != nil {
			t.Errorf("close bearer listener: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return bearer.server.Addr().String()
}

func dialBearer(t *testing.T, address, authority string) *quic.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, address, &tls.Config{
		InsecureSkipVerify: true, // self-signed test certificate
		ServerName:         authority,
		NextProtos:         []string{protocol.ALPN},
	}, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", authority, err)
	}
	t.Cleanup(func() {
		if err := conn.CloseWithError(0, "test done"); err != nil {
			t.Logf("close connection: %v", err)
		}
	})
	return conn
}

// send writes req on a new stream and returns the stream for the answer.
func send(t *testing.T, conn *quic.Conn, req protocol.Request) (*quic.Stream, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	if _, err := req.WriteTo(stream); err != nil {
		return nil, err
	}
	if err := stream.Close(); err != nil {
		return nil, err
	}
	return stream, nil
}

func exchange(t *testing.T, conn *quic.Conn, req protocol.Request) protocol.Response {
	t.Helper()
	stream, err := send(t, conn, req)
	if err != nil {
		t.Fatalf("send %s %s: %v", req.Verb, req.Path, err)
	}
	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Verb, req.Path, err)
	}
	return resp
}

func bearerRequest(verb, path, bearer string) protocol.Request {
	req := protocol.Request{Verb: verb, Path: path, Metadata: map[string]string{}}
	if bearer != "" {
		req.Metadata["auth"] = bearer
	}
	if verb == protocol.VerbPublish {
		req.Metadata["expected-version"] = "0"
		req.Body = "# Note\n"
	}
	return req
}

func TestBearerListenerAdmitsEveryRequestThroughTheGate(t *testing.T) {
	conn := dialBearer(t, openBearerListener(t, &scriptedGates{}), bearerAuthority)
	cases := []struct {
		name, verb, path, bearer, want string
	}{
		{"no bearer", protocol.VerbFetch, "/index.md", "", protocol.StatusUnauthorized},
		{"forged bearer", protocol.VerbFetch, "/index.md", "forged", protocol.StatusUnauthorized},
		{"identity the world does not admit", protocol.VerbFetch, "/index.md", "stranger", protocol.StatusNotPermitted},
		{"write under the grant", protocol.VerbPublish, "/notes/a.md", "writer", protocol.StatusCreated},
		{"read what was written", protocol.VerbFetch, "/notes/a.md", "reader", protocol.StatusOK},
		{"write outside the grant", protocol.VerbPublish, "/elsewhere.md", "writer", protocol.StatusNotPermitted},
		{"write with a read only grant", protocol.VerbPublish, "/notes/b.md", "reader", protocol.StatusNotPermitted},
		// cap reads /private on 6309; here the gate admits it as a bearer and
		// the handler never sees it, so the guarded path stays closed.
		{"capability token as a bearer", protocol.VerbFetch, "/private/x.md", "cap", protocol.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exchange(t, conn, bearerRequest(tc.verb, tc.path, tc.bearer)).Status; got != tc.want {
				t.Errorf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBearerListenerOpensOnce(t *testing.T) {
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, writeTokens(t, t.TempDir(), "alice"), true))
	server := &Server{config: h.config, certificates: h.certs, worlds: h.manager, logger: slog.New(slog.DiscardHandler)}
	gates := (&scriptedGates{}).gates
	if err := server.OpenBearerListener("127.0.0.1:0", gates); err != nil {
		t.Fatalf("first open: %v", err)
	}
	t.Cleanup(func() {
		if err := server.listeners[0].server.Close(); err != nil {
			t.Errorf("close bearer listener: %v", err)
		}
	})
	if err := server.OpenBearerListener("127.0.0.1:0", gates); err == nil {
		t.Fatal("a second bearer listener opened")
	}
}

func TestBearerListenerRefusesAConnectionWithoutAGate(t *testing.T) {
	conn := dialBearer(t, openBearerListener(t, &scriptedGates{}), otherAuthority)
	stream, err := send(t, conn, bearerRequest(protocol.VerbFetch, "/index.md", "writer"))
	if err == nil {
		_, err = protocol.ParseResponse(stream)
	}
	if err == nil {
		t.Fatal("a world the gates refuse answered a request")
	}
}

func TestBearerWatchEndsWhenTheBearerLapses(t *testing.T) {
	scripted := &scriptedGates{expires: time.Now().Add(300 * time.Millisecond)}
	conn := dialBearer(t, openBearerListener(t, scripted), bearerAuthority)
	stream, err := send(t, conn, bearerRequest(protocol.VerbWatch, "/", "reader"))
	if err != nil {
		t.Fatal(err)
	}
	reader := protocol.NewWatchReader(stream)
	ack, err := reader.Next()
	if err != nil || ack.Status != protocol.StatusOK {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	for {
		block, err := reader.Next()
		if err != nil {
			t.Fatalf("watch ended without a terminal block: %v", err)
		}
		if block.Status == protocol.StatusOK {
			continue // heartbeat
		}
		if block.Status != protocol.StatusUnauthorized {
			t.Fatalf("terminal = %+v, want unauthorized", block)
		}
		if _, err := block.Cursor(); err != nil {
			t.Errorf("terminal carries no cursor to resume from: %v", err)
		}
		return
	}
}
