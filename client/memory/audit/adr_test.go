package audit

import (
	"context"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/protocol"
)

func TestNextADRCountsEveryPageArchivedIncluded(t *testing.T) {
	store := &fetchtest.Client{ListFn: func(_ context.Context, r fetch.ListRequest) (fetch.Result, error) {
		if !r.IncludeArchived {
			t.Errorf("walk without archived records: %+v", r)
		}
		if r.Cursor == "" {
			return fetchtest.ListPage(r.Path, "0002-b.md", "0001-a.md", "0002-b.md"), nil
		}
		return fetchtest.ListPage(r.Path, "", "0004-retired.md", "drafts/", "notes.md"), nil
	}}
	next, err := NextADR(t.Context(), &ClientStore{Client: store, Host: "h", Token: "tok"}, "/cwd/adr/")
	if err != nil || next != "0005" {
		t.Fatalf("NextADR = %q, %v; want 0005 (0003 is a gap, 0004 archived)", next, err)
	}
	if len(store.ListCalls) != 2 || store.ListCalls[0].Token != "tok" {
		t.Errorf("calls = %+v", store.ListCalls)
	}
}

func TestNextADRStartsAtOneAndSurfacesAFailedListing(t *testing.T) {
	empty := &fetchtest.Client{}
	if next, err := NextADR(t.Context(), &ClientStore{Client: empty, Host: "h"}, "/cwd/adr/"); err != nil || next != "0001" {
		t.Errorf("empty directory: %q, %v", next, err)
	}
	// A directory not there yet holds no records either: the series starts.
	gone := &fetchtest.Client{ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}}
	if next, err := NextADR(t.Context(), &ClientStore{Client: gone, Host: "h"}, "/cwd/adr/"); err != nil || next != "0001" {
		t.Errorf("missing directory: %q, %v", next, err)
	}
	down := &fetchtest.Client{ListFn: func(context.Context, fetch.ListRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusUnauthorized}}, nil
	}}
	if _, err := NextADR(t.Context(), &ClientStore{Client: down, Host: "h"}, "/cwd/adr/"); err == nil {
		t.Error("a refused listing produced a number")
	}
}
