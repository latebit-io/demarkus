package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/memtest"
)

// brokerTokens is a tokens file as a broker leaves it: many minted hashes,
// each with its own read scope, new on every rotation.
func brokerTokens(generation int) []byte {
	var b strings.Builder
	for i := range 500 {
		fmt.Fprintf(&b, "[tokens.t%d]\nhash = \"sha256-%08d%08d\"\npaths = [\"/tenant-%d-%d/**\"]\noperations = [\"read\", \"publish\"]\n", i, generation, i, generation, i)
	}
	return []byte(b.String())
}

// Each published store holds its files' read patterns, replaced whole on
// every load: rotations and vanishings must not accumulate them.
func TestSourceHeapTracksTheCurrentFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tokens.toml")
	reload := func(source *Source, generation int) {
		if generation%5 == 4 {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(file, brokerTokens(generation), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := source.Reload(); err != nil {
			t.Fatalf("reload %d: %v", generation, err)
		}
	}
	if err := os.WriteFile(file, brokerTokens(0), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := OpenSource(SourceConfig{TokensFile: file, Optional: true})
	if err != nil {
		t.Fatal(err)
	}
	const warmup, cycles = 10, 100
	for generation := 1; generation <= warmup; generation++ {
		reload(source, generation)
	}
	growth := memtest.Retained(func() {
		for generation := warmup + 1; generation <= warmup+cycles; generation++ {
			reload(source, generation)
		}
	})
	runtime.KeepAlive(source)
	fileBytes := int64(len(brokerTokens(0)))
	t.Logf("heap grew %d bytes", growth)
	if limit := 4 * fileBytes; growth > limit {
		t.Errorf("heap grew %d bytes over %d reloads of a %d-byte tokens file, want under %d", growth, cycles, fileBytes, limit)
	}
}
