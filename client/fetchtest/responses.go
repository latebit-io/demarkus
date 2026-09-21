package fetchtest

import (
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol/render"
	"github.com/latebit-io/demarkus/protocol/wiretest"
)

// Golden is the named response the real server emitted; see protocol/wiretest.
func Golden(t testing.TB, name string) fetch.Result {
	t.Helper()
	return fetch.Result{Response: wiretest.Response(t, name)}
}

// Lookup is the server's rendering of a LOOKUP answer. match is the request's
// match key as the server would echo it; empty means it was not carried.
func Lookup(query, scope, match string, rows ...render.LookupRow) fetch.Result {
	return fetch.Result{Response: render.LookupResponse(query, scope, rows, match)}
}

// Versions is the server's rendering of a valid history of current versions.
func Versions(docPath string, current int) fetch.Result {
	modified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	history := make([]render.VersionEntry, 0, current)
	for v := current; v >= 1; v-- {
		history = append(history, render.VersionEntry{Version: v, Modified: modified})
	}
	return fetch.Result{Response: render.VersionsResponse(docPath, history, true)}
}
