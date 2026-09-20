package broker

import (
	"errors"
	"testing"
	"time"
)

// Unauthenticated callers must not be able to grow the pending maps without bound.
func TestPendingGrantStoresAreCapped(t *testing.T) {
	t.Run("device", func(t *testing.T) {
		s := newDeviceStore(nil, time.Minute, time.Second)
		s.maxPending = 3
		for i := range 3 {
			if _, _, _, err := s.Authorize(); err != nil {
				t.Fatalf("Authorize %d: %v", i, err)
			}
		}
		if _, _, _, err := s.Authorize(); !errors.Is(err, errGrantStoreFull) {
			t.Fatalf("Authorize past cap = %v, want errGrantStoreFull", err)
		}
		if len(s.codes) != 3 {
			t.Fatalf("store grew to %d entries past the cap", len(s.codes))
		}
	})
	t.Run("auth code", func(t *testing.T) {
		s := newAuthCodeStore(nil, time.Minute, time.Minute)
		s.maxPending = 3
		for i := range 3 {
			if _, err := s.Begin(&AuthCodeRequest{ClientID: "c"}); err != nil {
				t.Fatalf("Begin %d: %v", i, err)
			}
		}
		if _, err := s.Begin(&AuthCodeRequest{ClientID: "c"}); !errors.Is(err, errGrantStoreFull) {
			t.Fatalf("Begin past cap = %v, want errGrantStoreFull", err)
		}
		if len(s.pending) != 3 {
			t.Fatalf("store grew to %d entries past the cap", len(s.pending))
		}
	})
}

func TestAuthorizeBeginError(t *testing.T) {
	if code, _ := authorizeBeginError(errGrantStoreFull); code != "temporarily_unavailable" {
		t.Errorf("full store code = %q, want temporarily_unavailable", code)
	}
	if code, _ := authorizeBeginError(errors.New("rand failed")); code != "server_error" {
		t.Errorf("other failure code = %q, want server_error", code)
	}
}
