package gate

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/mcpfmt"
)

// Every host's write hook names exactly the tools the gate holds: a verb
// added to mcpfmt.StoreWriteTools without its matcher would skip the gate.
func TestHookMatchersNameTheStoreWrites(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..") // this package's repo root
	want := make([]string, 0, len(mcpfmt.StoreWriteTools))
	for tool := range mcpfmt.StoreWriteTools {
		want = append(want, strings.TrimPrefix(tool, "mark_"))
	}
	slices.Sort(want)
	verbs := regexp.MustCompile(`mark_\((?:\?:)?([a-z_|]+)\)|mark_publish(?:\|mark_[a-z_]+)+`)
	for _, file := range []string{
		"plugins/claude-code/.claude-plugin/plugin.json",
		"plugins/claude-code-knowledge/.claude-plugin/plugin.json",
		"plugins/cursor-memory/hooks/hooks.json",
		"plugins/cursor-knowledge/hooks/hooks.json",
		"plugins/opencode-memory/src/taint.ts",
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		var found [][]string
		for line := range strings.SplitSeq(string(raw), "\n") {
			// Cursor's after-hook tracks journal activity, not the gate.
			if strings.Contains(line, "mark-activity") {
				continue
			}
			found = append(found, verbs.FindAllStringSubmatch(line, -1)...)
		}
		if len(found) == 0 {
			t.Errorf("%s: no write matcher", file)
		}
		for _, m := range found {
			list := m[1]
			if list == "" {
				list = strings.ReplaceAll(m[0], "mark_", "")
			}
			got := strings.Split(list, "|")
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s: matcher %q names %v, want %v", file, m[0], got, want)
			}
		}
	}
}
