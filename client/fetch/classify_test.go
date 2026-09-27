package fetch

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/quic-go/quic-go"
)

type temporaryErr struct{}

func (temporaryErr) Error() string   { return "temporary" }
func (temporaryErr) Temporary() bool { return true }

// Every error this package returns is wrapped, so both classifiers unwrap. A
// timed out OpenStreamSync must evict, or the dead pooled connection wedges
// every later request; a timeout after the request went out is one stream's.
func TestClassifyErrors(t *testing.T) {
	reset := &quic.StreamError{StreamID: 4, ErrorCode: 1, Remote: true}
	tests := []struct {
		name         string
		err          error
		conn, stream bool
	}{
		{"nil", nil, false, false},
		{"open stream timeout", fmt.Errorf("open stream: %w", context.DeadlineExceeded), true, false},
		{"dial timeout", fmt.Errorf("dial host: %w", context.DeadlineExceeded), true, false},
		{"dial refused", errors.New("dial host: connection refused"), true, false},
		{"temporary", fmt.Errorf("send request: %w", temporaryErr{}), true, false},
		{"eof", errors.New("EOF"), true, false},
		{"peer closed connection", &sentError{cause: fmt.Errorf("read response: %w", &quic.ApplicationError{Remote: true})}, true, false},
		{"idle timeout while reading", &sentError{cause: fmt.Errorf("read response: %w", &quic.IdleTimeoutError{})}, true, false},
		{"stateless reset", fmt.Errorf("open stream: %w", &quic.StatelessResetError{}), true, false},
		{"peer reset stream", &sentError{cause: fmt.Errorf("read response: reading response: %w", reset)}, false, true},
		{"reset before send", fmt.Errorf("send request: %w", reset), false, true},
		{"read timeout after send", &sentError{cause: context.DeadlineExceeded}, false, true},
		{"response budget", ErrResponseBudget, false, false},
		{"malformed response", &sentError{cause: errors.New("read response: malformed frontmatter")}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isConnectionError(tt.err); got != tt.conn {
				t.Errorf("isConnectionError = %v, want %v", got, tt.conn)
			}
			if got := isStreamError(tt.err); got != tt.stream {
				t.Errorf("isStreamError = %v, want %v", got, tt.stream)
			}
			if got := isRetryable(tt.err); got != (tt.conn || tt.stream) {
				t.Errorf("isRetryable = %v, want %v", got, tt.conn || tt.stream)
			}
		})
	}
}

func TestIsTimeoutError_Unwraps(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Fatal("isTimeoutError(context.DeadlineExceeded) = false, want true")
	}
	wrapped := fmt.Errorf("open stream: %w", context.DeadlineExceeded)
	if !isTimeoutError(wrapped) {
		t.Fatalf("isTimeoutError(%q) = false, want true (must unwrap)", wrapped)
	}
	if isTimeoutError(errors.New("malformed response")) {
		t.Error("isTimeoutError(plain error) = true, want false")
	}
}

func TestIsTemporaryError_Unwraps(t *testing.T) {
	wrapped := fmt.Errorf("send request: %w", temporaryErr{})
	if !isTemporaryError(wrapped) {
		t.Fatalf("isTemporaryError(%q) = false, want true (must unwrap)", wrapped)
	}
	if isTemporaryError(errors.New("malformed response")) {
		t.Error("isTemporaryError(plain error) = true, want false")
	}
}
