package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadDraft(t *testing.T) {
	if got, err := readDraft(context.Background(), strings.NewReader("# Draft\n")); err != nil || got != "# Draft\n" {
		t.Errorf("readDraft = %q, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A pipe nobody writes to: the read never ends, the cancelled ctx does.
	pr, _ := io.Pipe()
	if _, err := readDraft(ctx, pr); !errors.Is(err, context.Canceled) {
		t.Errorf("open stdin under a cancelled ctx = %v, want context.Canceled", err)
	}
}
