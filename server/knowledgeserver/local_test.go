package knowledgeserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
)

func TestExchangeServesRoutedWorldInProcess(t *testing.T) {
	tokensDir := t.TempDir()
	tokens := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	server := &Server{worlds: h.manager}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := server.Exchange(ctx, "alice.memory.svc.cluster.local", protocol.Request{Verb: protocol.VerbList, Path: "/"})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Status != protocol.StatusOK {
		t.Fatalf("status = %q, want %q (body %q)", resp.Status, protocol.StatusOK, resp.Body)
	}

	resp, err = server.Exchange(ctx, "alice.memory.svc.cluster.local", protocol.Request{Verb: protocol.VerbFetch, Path: "/missing.md"})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Status != protocol.StatusNotFound {
		t.Fatalf("status = %q, want %q", resp.Status, protocol.StatusNotFound)
	}
}

func TestExchangeRefusesUnknownAuthority(t *testing.T) {
	tokensDir := t.TempDir()
	tokens := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	server := &Server{worlds: h.manager}

	_, err := server.Exchange(context.Background(), "bob.memory.svc.cluster.local", protocol.Request{Verb: protocol.VerbList, Path: "/"})
	if !errors.Is(err, ErrUnknownAuthority) {
		t.Fatalf("err = %v, want ErrUnknownAuthority", err)
	}
}

func TestExchangeHonoursContext(t *testing.T) {
	tokensDir := t.TempDir()
	tokens := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	server := &Server{worlds: h.manager}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := server.Exchange(ctx, "alice.memory.svc.cluster.local", protocol.Request{Verb: protocol.VerbList, Path: "/"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestExchangeHonoursAGrantOnTheContext(t *testing.T) {
	tokensDir := t.TempDir()
	tokens := writeTokens(t, tokensDir, "alice")
	h := newWorldsHarness(t, "worlds:\n"+worldFragment("alice", testWorldID, tokens, true))
	server := &Server{worlds: h.manager}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const authority = "alice.memory.svc.cluster.local"
	publish := protocol.Request{Verb: protocol.VerbPublish, Path: "/notes/a.md", Body: "# A\n", Metadata: map[string]string{"expected-version": "0"}}

	resp, err := server.Exchange(ctx, authority, publish)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Status != protocol.StatusUnauthorized {
		t.Fatalf("tokenless publish over Exchange: status = %q, want %q", resp.Status, protocol.StatusUnauthorized)
	}

	granted := protocol.WithGrant(ctx, protocol.Grant{Label: "alice@example.com", Paths: []string{"/notes/**"}})
	resp, err = server.Exchange(granted, authority, publish)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Status != protocol.StatusCreated {
		t.Fatalf("granted publish: status = %q, want %q (body %q)", resp.Status, protocol.StatusCreated, resp.Body)
	}

	outside := publish
	outside.Path = "/private/b.md"
	resp, err = server.Exchange(granted, authority, outside)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Status != protocol.StatusNotPermitted {
		t.Fatalf("publish outside the grant: status = %q, want %q", resp.Status, protocol.StatusNotPermitted)
	}
}
