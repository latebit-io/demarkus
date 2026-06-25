package fetch

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Every error this package returns is wrapped (fmt.Errorf("open stream: %w", …)
// etc.), so the transient-failure classifier must unwrap. The bug it guards
// against: a bare type assertion on the *wrapper* misses the underlying
// context.DeadlineExceeded, classes a timed-out OpenStreamSync as non-transient,
// and doWithRetry then never evicts+redials the dead pooled connection — wedging
// every later request on that host.
func TestIsTransientError_UnwrapsWrappedTimeout(t *testing.T) {
	wrapped := fmt.Errorf("open stream: %w", context.DeadlineExceeded)

	if !isTimeoutError(wrapped) {
		t.Fatalf("isTimeoutError(%q) = false, want true (must unwrap to context.DeadlineExceeded)", wrapped)
	}
	if !isTransientError(wrapped) {
		t.Fatalf("isTransientError(%q) = false, want true", wrapped)
	}
}

func TestIsTransientError_BareTimeout(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Fatal("isTimeoutError(context.DeadlineExceeded) = false, want true")
	}
}

type temporaryErr struct{}

func (temporaryErr) Error() string   { return "temporary" }
func (temporaryErr) Temporary() bool { return true }

func TestIsTransientError_UnwrapsWrappedTemporary(t *testing.T) {
	wrapped := fmt.Errorf("send request: %w", temporaryErr{})

	if !isTemporaryError(wrapped) {
		t.Fatalf("isTemporaryError(%q) = false, want true (must unwrap)", wrapped)
	}
	if !isTransientError(wrapped) {
		t.Fatalf("isTransientError(%q) = false, want true", wrapped)
	}
}

func TestIsTransientError_NonTransient(t *testing.T) {
	err := errors.New("malformed response")

	if isTimeoutError(err) {
		t.Error("isTimeoutError(plain error) = true, want false")
	}
	if isTemporaryError(err) {
		t.Error("isTemporaryError(plain error) = true, want false")
	}
	if isTransientError(err) {
		t.Error("isTransientError(plain error) = true, want false")
	}
}
