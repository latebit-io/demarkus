package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/backend/backendtest"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// bodySettle bounds how long a backend may answer body lookups as catalog
// fallback while it builds or catches up its section index (ADR 0036).
const bodySettle = 5 * time.Second

// settle calls try until it reports done or bodySettle passes.
func settle(try func() (done bool)) {
	deadline := time.Now().Add(bodySettle)
	for !try() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// SettledLookup is Lookup, retrying a body lookup answered as catalog
// fallback until bodySettle passes; any other answer returns at once.
func SettledLookup(d backendtest.Direct, query string, opts catalog.Options) (results []catalog.Result, err error) {
	settle(func() bool {
		results, err = d.Lookup(query, opts)
		return !errors.Is(err, backend.ErrBodyMatchUnavailable)
	})
	return results, err
}

// sendSettled is Send, resending a body LOOKUP answered as catalog fallback
// until bodySettle passes; every other request is sent once.
func sendSettled(t testing.TB, h *handler.Handler, req protocol.Request) (resp protocol.Response) {
	t.Helper()
	settle(func() bool {
		resp = Send(t, h, req)
		return req.Verb != protocol.VerbLookup || req.Metadata["match"] != "body" || resp.Metadata["match"] != "catalog"
	})
	return resp
}
