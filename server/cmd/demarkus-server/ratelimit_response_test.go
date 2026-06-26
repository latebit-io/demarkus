package main

import (
	"bytes"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// A rate-limited rejection must send a real, parseable response carrying the
// rate-limited status — never an empty/statusless close, which a client cannot
// tell apart from a dead connection (the bug that silently blanked crawl nodes).
func TestWriteRateLimited(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRateLimited(&buf); err != nil {
		t.Fatalf("writeRateLimited: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("rate-limited reply is empty; client cannot distinguish it from a dead connection")
	}

	resp, err := protocol.ParseResponse(&buf)
	if err != nil {
		t.Fatalf("ParseResponse on rate-limited reply: %v", err)
	}
	if resp.Status != protocol.StatusRateLimited {
		t.Fatalf("status = %q, want %q", resp.Status, protocol.StatusRateLimited)
	}
}
