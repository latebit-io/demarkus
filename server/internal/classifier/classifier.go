// Package classifier asks a model whether a document body carries a prompt
// injection, before the write commits and under a deadline. It never refuses:
// a verdict becomes a server-owned metadata stamp the read path and doctor show.
package classifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/latebit-io/demarkus/protocol"
)

// Judge reports whether body carries a prompt injection, with a one-line reason.
type Judge func(ctx context.Context, path, body string) (injection bool, reason string, err error)

// Limits bound a Stamper; knowledgeconfig.SetDefaults fills them.
type Limits struct {
	Timeout     time.Duration // per body
	MaxBytes    int           // bodies above it are stamped unchecked, never cut
	MaxInFlight int           // concurrent calls; more are stamped unchecked
}

const (
	// minBytes skips bodies too short to carry an attack worth a call.
	minBytes = 64
	// maxReason bounds the stamp under the metadata byte budget.
	maxReason = 160
)

// Stamper turns a judge into the write path's stamp.
type Stamper struct {
	judge    Judge
	source   string
	timeout  time.Duration
	maxBytes int
	slots    chan struct{}
}

// New stamps with judge, naming source in a verdict.
func New(judge Judge, source string, limits Limits) *Stamper {
	return &Stamper{judge: judge, source: source, timeout: limits.Timeout, maxBytes: limits.MaxBytes, slots: make(chan struct{}, limits.MaxInFlight)}
}

// Stamp is the metadata value a write carries for body: "injection: <reason>
// (<source>)", "unchecked: <why>", or "" for a benign or skipped body.
func (s *Stamper) Stamp(ctx context.Context, path, body string) string {
	switch {
	case len(body) < minBytes:
		return ""
	case len(body) > s.maxBytes:
		return fmt.Sprintf("%s: %d bytes is over the classifier's %d", protocol.ClassifierUnchecked, len(body), s.maxBytes)
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return protocol.ClassifierUnchecked + ": classifier busy"
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	injection, reason, err := s.judge(ctx, path, body)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		return protocol.ClassifierUnchecked + ": classifier timed out"
	case err != nil:
		return protocol.ClassifierUnchecked + ": " + clip(err.Error())
	case injection:
		return fmt.Sprintf("%s: %s (%s)", protocol.ClassifierInjection, clip(reason), s.source)
	}
	return ""
}

// clip makes text one line within maxReason bytes, cut on a rune boundary.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxReason {
		return s
	}
	n := maxReason
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
