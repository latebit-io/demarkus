package storefmt

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// An APPEND keeps the document's metadata but not its last author: the
// identity keys describe the write that carried them.
func TestAppendDoesNotInheritIdentity(t *testing.T) {
	base := map[string]string{"tags": "a", protocol.MetaAgent: "cli", protocol.MetaUser: "alice@example.com"}
	got := PrepareAppendMeta("/doc.md", base, nil)
	if got["tags"] != "a" || got[protocol.MetaAgent] != "" || got[protocol.MetaUser] != "" {
		t.Errorf("append with no identity = %v, want tags kept and no agent or user", got)
	}
	got = PrepareAppendMeta("/doc.md", base, map[string]string{protocol.MetaAgent: "mcp", protocol.MetaUser: "bob@example.com"})
	if got[protocol.MetaAgent] != "mcp" || got[protocol.MetaUser] != "bob@example.com" {
		t.Errorf("append with its own identity = %v, want the append's", got)
	}
	if base[protocol.MetaUser] != "alice@example.com" {
		t.Error("the base map was changed")
	}
}
