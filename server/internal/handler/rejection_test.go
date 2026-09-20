package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

// refusingStore fails every mutation with one error, as a backend would.
type refusingStore struct {
	DocumentStore
	err error
}

func (s *refusingStore) WriteVersion(string, int, []byte, map[string]string) (*store.Document, error) {
	return nil, s.err
}

func (s *refusingStore) Append(string, int, []byte, map[string]string) (*store.Document, error) {
	return nil, s.err
}

func (s *refusingStore) ArchiveResult(string, bool) (*store.Document, bool, error) {
	return nil, false, s.err
}

type violationsError struct{}

func (violationsError) Error() string            { return "policy block: 1 violation(s)" }
func (violationsError) Unwrap() error            { return storagebackend.ErrRejected }
func (violationsError) RejectionMessage() string { return "missing-tags" }

// A refusal the client can correct must not read as a server fault.
func TestWriteRejectionStatusMapping(t *testing.T) {
	secret := "test-secret-key"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/*"}, Operations: []string{"publish"}},
	})
	authBlock := "auth: " + secret + "\n"

	errs := []struct {
		name     string
		err      error
		status   string
		contains string
	}{
		{name: "path collision", err: fmt.Errorf("cannot publish /a.md: %w", store.ErrPathCollision), status: protocol.StatusBadRequest, contains: "cannot publish"},
		{name: "quota", err: fmt.Errorf("document %w: limit 10", storagebackend.ErrQuota), status: protocol.StatusNotPermitted, contains: "quota"},
		{name: "policy violations", err: violationsError{}, status: protocol.StatusBadRequest, contains: "missing-tags"},
		{name: "unclassified", err: errors.New("bucket unreachable"), status: protocol.StatusServerError, contains: "internal error"},
	}
	requests := []struct {
		verb    string
		request string
	}{
		{verb: "PUBLISH", request: "PUBLISH /a.md\n---\n" + authBlock + "expected-version: 0\n---\n# A\n"},
		{verb: "APPEND", request: "APPEND /a.md\n---\n" + authBlock + "expected-version: 1\n---\nmore\n"},
		{verb: "ARCHIVE", request: "ARCHIVE /a.md\n---\n" + authBlock + "---\n"},
	}
	for _, e := range errs {
		for _, r := range requests {
			t.Run(e.name+"/"+r.verb, func(t *testing.T) {
				b := fileBackend(t)
				seedBackend(t, b, map[string]string{"a.md": "# A\n"})
				h := newHandler(b, ts)
				h.Store = &refusingStore{DocumentStore: b.Store, err: e.err}

				stream := newMockStream(r.request)
				h.HandleStream(stream)
				resp, err := protocol.ParseResponse(&stream.output)
				if err != nil {
					t.Fatalf("parse response: %v", err)
				}
				if resp.Status != e.status {
					t.Fatalf("status = %q, want %q (body %q)", resp.Status, e.status, resp.Body)
				}
				if !strings.Contains(resp.Body, e.contains) {
					t.Errorf("body %q does not contain %q", resp.Body, e.contains)
				}
			})
		}
	}
}
