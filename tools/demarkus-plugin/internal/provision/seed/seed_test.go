package seed

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// mockSeedClient records Publish calls and returns canned responses.
type publishCall struct {
	path, body string
	expected   int
}

type mockSeedClient struct {
	fetchStatus   string
	fetchErr      error
	publishStatus string
	published     []publishCall
}

func TestSeedDocPublishesThroughProtocol(t *testing.T) {
	// Already served (or archived, or unreachable): never publishes.
	for _, tt := range []struct {
		name string
		mock *mockSeedClient
	}{
		{"already served", &mockSeedClient{fetchStatus: protocol.StatusOK}},
		{"archived", &mockSeedClient{fetchStatus: protocol.StatusArchived}},
		{"server unreachable", &mockSeedClient{fetchErr: errors.New("dial refused")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			seedDoc(t.Context(), tt.mock, seedSpec{Host: "localhost:1", Token: "tok", Name: "index.md"})
			if len(tt.mock.published) != 0 {
				t.Errorf("published %d docs, want 0", len(tt.mock.published))
			}
		})
	}

	// Not found: publishes the embedded seed, create-only.
	m := &mockSeedClient{fetchStatus: protocol.StatusNotFound}
	seedDoc(t.Context(), m, seedSpec{Host: "localhost:1", Token: "tok", Name: "index.md"})
	if len(m.published) != 1 {
		t.Fatalf("published %d docs, want 1", len(m.published))
	}
	if m.published[0].path != "/index.md" || m.published[0].expected != 0 {
		t.Errorf("publish = %+v, want path /index.md expected-version 0", m.published[0])
	}
	if !strings.Contains(m.published[0].body, "# Projects") {
		t.Errorf("seeded content does not look like index.md: %q", m.published[0].body[:min(40, len(m.published[0].body))])
	}
}

func (m *mockSeedClient) Fetch(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	if m.fetchErr != nil {
		return fetch.Result{}, m.fetchErr
	}
	return fetch.Result{Response: protocol.Response{Status: m.fetchStatus}}, nil
}

func (m *mockSeedClient) Publish(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	m.published = append(m.published, publishCall{r.Path, r.Body, r.ExpectedVersion})
	st := m.publishStatus
	if st == "" {
		st = protocol.StatusCreated
	}
	return fetch.Result{Response: protocol.Response{Status: st}}, nil
}
