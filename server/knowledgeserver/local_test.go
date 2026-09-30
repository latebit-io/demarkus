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
