package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
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
		heard := logs.Len()
		h.HandleStream(context.Background(), deafStream{strings.NewReader("PUBLISH /deaf.md" + head + "---\n# Unheard\n")})

		logged := logs.String()
		with, without := durationsByMessage(t, logged)
		for msg, want := range timed {
			if with[msg] != want || without[msg] == want {
				t.Errorf("%q lines with duration_ms: %t, without: %t; want every one with it: %t\n%s", msg, with[msg], without[msg], want, logged)
			}
		}

		// The deaf request alone: its write committed, then the send failed.
		unheard := logged[heard:]
		with, without = durationsByMessage(t, unheard)
		if !with["publish"] || without["publish"] {
			t.Errorf("deaf request logged no timed publish:\n%s", unheard)
		}
		if !without["write response failed"] || with["write response failed"] {
			t.Errorf("deaf request logged no untimed write response failed:\n%s", unheard)
		}
	})
}

// durationsByMessage reports which messages in JSON log lines carry
// duration_ms and which appear without it.
func durationsByMessage(t *testing.T, logged string) (with, without map[string]bool) {
	t.Helper()
	with, without = map[string]bool{}, map[string]bool{}
	type record struct {
		Msg  string
		Took *int64 `json:"duration_ms"`
	}
	for _, r := range logRecords[record](t, logged) {
		if r.Took != nil {
			with[r.Msg] = true
		} else {
			without[r.Msg] = true
		}
	}
	return with, without
}

// logRecords decodes a JSON handler's lines.
func logRecords[T any](t *testing.T, logged string) []T {
	t.Helper()
	var records []T
	for line := range strings.Lines(logged) {
		var record T
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

// deafStream fails every write of the response.
type deafStream struct{ io.Reader }

func (deafStream) Write([]byte) (int, error) { return 0, errors.New("peer gone") }
func (deafStream) Close() error              { return nil }

// A write refused for what it carries is audit-logged with its reason and the
// answer's message, before the store or by it, so an operator can see why.
func TestWriteRejectionsAreAuditLogged(t *testing.T) {
	const secret = "test-reject-secret"
	tokenStore := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/*"}, Operations: []string{"publish", "append"}},
	})
	head := "\n---\nauth: " + secret + "\n"
	wide := "tags: " + strings.Repeat("x", protocol.MaxMetaBytes) + "\n"
	nearFull := "notes: " + strings.Repeat("n", protocol.MaxMetaBytes-64) + "\n"
	tooLarge := fmt.Sprintf("bytes, max %d)", protocol.MaxMetaBytes)
	cases := []struct{ request, msg, reason, detail string }{
		{"PUBLISH /doc.md" + head + wide + "---\n# One\n", "publish rejected", "invalid metadata", tooLarge},
		{"PUBLISH /doc.md" + head + "version: 3\n---\n# One\n", "publish rejected", "invalid metadata", `metadata key "version" is reserved`},
		{"PUBLISH /doc.md" + head + "expected-version: x\n---\n# One\n", "publish rejected", "invalid expected-version", "invalid expected-version"},
		{"APPEND /doc.md" + head + wide + "expected-version: 1\n---\nmore\n", "append rejected", "invalid metadata", tooLarge},
		{"APPEND /doc.md" + head + "---\nmore\n", "append rejected", "invalid expected-version", "APPEND requires expected-version metadata"},
		{"APPEND /doc.md" + head + "expected-version: 1\n---\n", "append rejected", "empty body", "append requires a body"},
		// Each request fits; the store refuses what the append merges to.
		{"PUBLISH /doc.md" + head + nearFull + "---\n# One\n", "publish", "", ""},
		{"APPEND /doc.md" + head + "owner: " + strings.Repeat("o", 64) + "\nexpected-version: 1\n---\nmore\n", "append rejected", "merged metadata invalid", tooLarge},
	}
	type record struct {
		Msg, Reason, Detail string
	}
	forEachBackend(t, func(t *testing.T, newBackend backendFactory) {
		var logs bytes.Buffer
		h := mustNew(Config{
			Store:         newBackend(t).Store,
			Logger:        slog.New(slog.NewJSONHandler(&logs, nil)),
			GetTokenStore: func() *auth.TokenStore { return tokenStore },
		})
		for _, c := range cases {
			logs.Reset()
			stream := newMockStream(c.request)
			h.HandleStream(context.Background(), stream)
			resp, err := protocol.ParseResponse(&stream.output)
			if err != nil || !strings.Contains(resp.Refusal(), c.detail) {
				t.Errorf("%q answered %q (%v), want %q", c.request, resp.Refusal(), err, c.detail)
			}
			if !slices.ContainsFunc(logRecords[record](t, logs.String()), func(r record) bool {
				return r.Msg == c.msg && r.Reason == c.reason && strings.Contains(r.Detail, c.detail)
			}) {
				t.Errorf("%q logged no %q with reason %q, detail %q:\n%s", c.request, c.msg, c.reason, c.detail, logs.String())
			}
		}
	})
}
