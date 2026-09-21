package fetch

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestLookupSendsMatchOnlyWhenSet(t *testing.T) {
	seen := make(chan map[string]string, 2)
	host := startTestServer(t, func(req protocol.Request) protocol.Response {
		seen <- req.Metadata
		return protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"matches": "0", "match": req.Metadata["match"]}}
	})
	c := NewClient(Options{Insecure: true})
	defer c.Close()
	if _, err := c.Lookup(t.Context(), LookupRequest{Host: host, Scope: "/", Query: "auth"}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := c.Lookup(t.Context(), LookupRequest{Host: host, Scope: "/", Query: "auth", Match: MatchBody}); err != nil {
		t.Fatalf("lookup body: %v", err)
	}
	first, second := <-seen, <-seen
	if _, present := first["match"]; present || second["match"] != MatchBody {
		t.Errorf("match metadata sent = %q / %q, want absent then body", first["match"], second["match"])
	}
}

func TestLookupRejectsUnknownMatchBeforeDialing(t *testing.T) {
	c := NewClient(Options{Insecure: true})
	defer c.Close()
	for _, bad := range []string{"BODY", "bogus", "body,catalog"} {
		// An unroutable host proves the request never left: a dial would fail
		// with a different error.
		_, err := c.Lookup(t.Context(), LookupRequest{Host: "mark://127.0.0.1:1", Scope: "/", Query: "auth", Match: bad})
		if err == nil || !strings.Contains(err.Error(), "match must be") {
			t.Errorf("Lookup(match=%q) err = %v, want validation error", bad, err)
		}
	}
}

func TestAnsweredFromCatalog(t *testing.T) {
	body := LookupRequest{Match: MatchBody}
	ok := Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"match": "body"}}}
	silent := Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{}}}
	failed := Result{Response: protocol.Response{Status: protocol.StatusBadRequest}}
	if AnsweredFromCatalog(body, ok) || !AnsweredFromCatalog(body, silent) || AnsweredFromCatalog(body, failed) || AnsweredFromCatalog(LookupRequest{}, silent) {
		t.Error("AnsweredFromCatalog: want true only for an ok body request lacking the echo")
	}
}
