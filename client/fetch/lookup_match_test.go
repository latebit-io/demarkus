package fetch

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestLookupSendsMatchOnlyWhenSet(t *testing.T) {
	var seen []map[string]string
	host := startTestServer(t, func(req protocol.Request) protocol.Response {
		seen = append(seen, req.Metadata)
		return protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"matches": "0", "match": req.Metadata["match"]}}
	})
	c := NewClient(Options{Insecure: true})
	defer c.Close()
	if _, err := c.Lookup(host, "/", "auth", "", LookupOptions{}); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := c.Lookup(host, "/", "auth", "", LookupOptions{Match: MatchBody}); err != nil {
		t.Fatalf("lookup body: %v", err)
	}
	if _, present := seen[0]["match"]; present || seen[1]["match"] != MatchBody {
		t.Errorf("match metadata sent = %q / %q, want absent then body", seen[0]["match"], seen[1]["match"])
	}
}

func TestAnsweredFromCatalog(t *testing.T) {
	body := LookupOptions{Match: MatchBody}
	ok := Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{"match": "body"}}}
	silent := Result{Response: protocol.Response{Status: protocol.StatusOK, Metadata: map[string]string{}}}
	failed := Result{Response: protocol.Response{Status: protocol.StatusBadRequest}}
	if AnsweredFromCatalog(body, ok) || !AnsweredFromCatalog(body, silent) || AnsweredFromCatalog(body, failed) || AnsweredFromCatalog(LookupOptions{}, silent) {
		t.Error("AnsweredFromCatalog: want true only for an ok body request lacking the echo")
	}
}
