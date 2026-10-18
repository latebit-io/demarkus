// Package wiretest serves the server's real wire responses to consumer tests.
// The goldens are emitted by server/internal/storetest; regenerate there with
// go test ./internal/storetest -run TestFileStoreWireGoldens -update.
package wiretest

import (
	"bytes"
	"embed"
	"fmt"
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
	resp, err := Load(name)
	if err != nil {
		t.Fatalf("%v (known: %s)", err, strings.Join(Names(t), ", "))
	}
	return resp
}

// Load is Response for a consumer without a testing.TB, such as a generator
// that renders fixtures from the server's answers.
func Load(name string) (protocol.Response, error) {
	wire, err := goldens.ReadFile("testdata/" + name + ".golden")
	if err != nil {
		return protocol.Response{}, fmt.Errorf("wire golden %q: %w", name, err)
	}
	resp, err := protocol.ParseResponse(bytes.NewReader(wire))
	if err != nil {
		return protocol.Response{}, fmt.Errorf("wire golden %q does not parse: %w", name, err)
	}
	return resp, nil
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
