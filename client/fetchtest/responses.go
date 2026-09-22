package fetchtest

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
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

// Head is a live document at version, as a FETCH of it answers.
func Head(body string, version int, meta map[string]string) fetch.Result {
	m := map[string]string{"version": strconv.Itoa(version)}
	maps.Copy(m, meta)
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: body, Metadata: m}}
}

// LostResponse is a write that was sent and never answered.
func LostResponse() error { return fmt.Errorf("read response: %w", protocol.ErrOutcomeUnknown) }

// History answers FETCH for a document with the given versions, oldest first:
// docPath is the newest, docPath/vN is version N, anything else is not found.
// Every version carries meta, as versions written by one publisher do.
func History(docPath string, meta map[string]string, bodies ...string) func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	return func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		for i, body := range bodies {
			version := i + 1
			if r.Path == protocol.VersionPath(docPath, version) || (version == len(bodies) && r.Path == docPath) {
				return Head(body, version, meta), nil
			}
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
}

// Archived is a FETCH of an archived document as the server answers it: the
// status and nothing else, no version (see the fetch-archived wire golden).
func Archived() fetch.Result {
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusArchived}}
}
