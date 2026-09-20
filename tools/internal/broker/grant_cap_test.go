package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
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

func TestAuthCodeStoreCancelFreesThePendingSlot(t *testing.T) {
	s := newAuthCodeStore(nil, time.Minute, time.Minute)
	s.maxPending = 1
	id, err := s.Begin(&AuthCodeRequest{ClientID: "c"})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	s.Cancel(id)
	if _, ok := s.LookupPending(id); ok {
		t.Error("canceled grant is still pending")
	}
	if _, err := s.Begin(&AuthCodeRequest{ClientID: "c"}); err != nil {
		t.Errorf("Begin after Cancel = %v, want the slot to be free", err)
	}
}

// A full store answers device clients in the OAuth JSON error shape, with Retry-After.
func TestDeviceAuthorizeFullStoreResponse(t *testing.T) {
	srv, broker := newTestServer(t, deviceTestConfig(), &fakeVerifier{}, fake.NewSimpleClientset())
	broker.deviceStore.maxPending = 0

	resp, err := testClient(srv).PostForm(srv.URL+"/device/authorize", url.Values{"client_id": {"demarkus-cli"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	var out deviceTokenError
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || out.Error != "temporarily_unavailable" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d error %q retry-after %q", resp.StatusCode, out.Error, resp.Header.Get("Retry-After"))
	}
}
