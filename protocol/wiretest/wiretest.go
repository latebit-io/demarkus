// Package wiretest serves the server's real wire responses to consumer tests.
// The goldens are emitted by server/internal/storetest; regenerate there with
// go test ./internal/storetest -run TestFileStoreWireGoldens -update.
package wiretest

import (
	"bytes"
	"embed"
	"sort"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

//go:embed testdata/*.golden
var goldens embed.FS

// Response is the named golden response, for example "list" or "versions".
func Response(t testing.TB, name string) protocol.Response {
	t.Helper()
	wire, err := goldens.ReadFile("testdata/" + name + ".golden")
	if err != nil {
		t.Fatalf("wire golden %q: %v (known: %s)", name, err, strings.Join(Names(t), ", "))
	}
	resp, err := protocol.ParseResponse(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("wire golden %q does not parse: %v", name, err)
	}
	return resp
}

// Names lists every golden, sorted.
func Names(t testing.TB) []string {
	t.Helper()
	entries, err := goldens.ReadDir("testdata")
	if err != nil {
		t.Fatalf("read embedded wire goldens: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".golden"))
	}
	sort.Strings(names)
	return names
}
