package fetch

import (
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestParseListPageMetadata(t *testing.T) {
	page, err := ParseListPageMetadata(protocol.Response{
		Status: protocol.StatusOK,
		Metadata: map[string]string{
			"entries":     "2",
			"complete":    "false",
			"next-cursor": "next",
		},
	})
	if err != nil || page != (ListPageMetadata{Entries: 2, NextCursor: "next"}) {
		t.Fatalf("page = (%+v, %v)", page, err)
	}

	page, err = ParseListPageMetadata(protocol.Response{
		Status:   protocol.StatusOK,
		Metadata: map[string]string{"entries": "0", "complete": "true"},
	})
	if err != nil || !page.Complete || page.Entries != 0 {
		t.Fatalf("terminal page = (%+v, %v)", page, err)
	}
}

func TestParseListPageMetadataRejectsInconsistentState(t *testing.T) {
	tests := []struct {
		name string
		resp protocol.Response
		want error
	}{
		{"legacy", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "1"}}, ErrListCompletenessUnknown},
		{"bad entries", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "x", "complete": "true"}}, nil},
		{"too many entries", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "1001", "complete": "true"}}, nil},
		{"missing cursor", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "1", "complete": "false"}}, nil},
		{"no progress", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "0", "complete": "false", "next-cursor": "x"}}, nil},
		{"terminal cursor", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "1", "complete": "true", "next-cursor": "x"}}, nil},
		{"oversized body", protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"entries": "1", "complete": "true"}, Body: string(make([]byte, protocol.MaxBodyLength+1))}, nil},
		{"non ok", protocol.Response{Status: protocol.StatusNotFound}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseListPageMetadata(tt.resp)
			if err == nil {
				t.Fatal("invalid metadata accepted")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestListWithOptionsWritesPaginationMetadata(t *testing.T) {
	requests := make(chan protocol.Request, 1)
	host := startTestServer(t, func(req protocol.Request) protocol.Response {
		requests <- req
		return protocol.Response{
			Status:   protocol.StatusOK,
			Metadata: map[string]string{"entries": "0", "complete": "true"},
		}
	})
	client := NewClient(Options{Insecure: true})
	defer client.Close()

	_, err := client.ListWithOptions(host, "/docs", "secret", ListOptions{
		IncludeArchived: true,
		Cursor:          "next",
		PageSize:        25,
	})
	if err != nil {
		t.Fatalf("ListWithOptions: %v", err)
	}
	req := <-requests
	for key, want := range map[string]string{
		"auth": "secret", "include-archived": "true", "cursor": "next", "page-size": "25",
	} {
		if req.Metadata[key] != want {
			t.Errorf("metadata[%q] = %q, want %q", key, req.Metadata[key], want)
		}
	}
	if _, err := client.ListWithOptions(host, "/docs", "", ListOptions{PageSize: protocol.MaxListPageSize + 1}); err == nil {
		t.Fatal("oversized page request accepted")
	}
}
