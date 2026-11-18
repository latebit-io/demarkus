package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/auth"
)

// The line that records what the store answered a write says how long the
// write took, on success and on refusal. The request line, a prune line, a
// refusal made before the store and a failed response send do not.
func TestWriteOutcomeLogsDuration(t *testing.T) {
	const secret = "test-duration-secret"
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/*"}, Operations: []string{"publish", "append", "archive"}},
	})
	head := "\n---\nauth: " + secret + "\n"
	requests := []string{
		"PUBLISH /doc.md" + head + "---\n# One\n",
		"PUBLISH /doc.md" + head + "expected-version: 0\n---\n# Two\n",
		"APPEND /doc.md" + head + "expected-version: 1\n---\nmore\n",
		"ARCHIVE /doc.md" + head + "---\n",
		"PUBLISH /doc.md" + head + "---\n",
		"PUBLISH /doc.md" + head + "---\n",
		"ARCHIVE /missing.md" + head + "---\n",
		"APPEND /missing.md" + head + "expected-version: 1\n---\nmore\n",
		"PUBLISH /doc.md" + head + "retention: 1\n---\n# Three\n",
		"PUBLISH /doc.txt" + head + "---\nnot markdown\n",
	}
	// Message to whether its lines carry duration_ms; the rejection here is
	// the non-markdown path, refused before the store.
	timed := map[string]bool{
		"publish": true, "publish conflict": true, "append": true,
		"archive": true, "unarchive": true, "unarchive unchanged": true,
		"archive failed": true, "append failed": true,
		"request": false, "prune": false, "publish rejected": false,
		"write response failed": false,
	}

	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		var logs bytes.Buffer
		h := mustNew(Config{
			Store:         newBackend(t).Store,
			Logger:        slog.New(slog.NewJSONHandler(&logs, nil)),
			GetTokenStore: func() *auth.TokenStore { return tokenStore },
		})
		for _, request := range requests {
			h.HandleStream(context.Background(), newMockStream(request))
		}
		h.HandleStream(context.Background(), deafStream{strings.NewReader("PUBLISH /deaf.md" + head + "---\n# Unheard\n")})

		logged := logs.String()
		with, without := map[string]bool{}, map[string]bool{}
		for line := range strings.Lines(logged) {
			var record struct {
				Msg  string
				Took *int64 `json:"duration_ms"`
			}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			if record.Took != nil {
				with[record.Msg] = true
			} else {
				without[record.Msg] = true
			}
		}
		for msg, want := range timed {
			if with[msg] != want || without[msg] == want {
				t.Errorf("%q lines with duration_ms: %t, without: %t; want every one with it: %t\n%s", msg, with[msg], without[msg], want, logged)
			}
		}
	})
}

// deafStream fails every write of the response.
type deafStream struct{ io.Reader }

func (deafStream) Write([]byte) (int, error) { return 0, errors.New("peer gone") }
func (deafStream) Close() error              { return nil }
