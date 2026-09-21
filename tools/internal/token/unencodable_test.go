package token

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Go %q emits escapes such as \a and \v that TOML rejects; one such label must
// not leave the file unparseable for every other token.
func TestAppendRejectsUnencodableEntry(t *testing.T) {
	good := &Entry{Hash: "sha256-aa", Paths: []string{"/*"}, Operations: []string{"publish"}}
	tests := []struct {
		name  string
		label string
		entry *Entry
	}{
		{name: "vertical tab in label", label: "a\vb", entry: good},
		{name: "bell in label", label: "a\ab", entry: good},
		{name: "control char in path", label: "ok", entry: &Entry{Hash: "sha256-aa", Paths: []string{"/a\v"}, Operations: []string{"publish"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.toml")
			if err := AppendEntry(path, "first", good); err != nil {
				t.Fatalf("seed: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			if err := AppendEntry(path, tt.label, tt.entry); !errors.Is(err, ErrEntryUnencodable) {
				t.Fatalf("AppendEntry err = %v, want ErrEntryUnencodable", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("tokens file changed by a refused append")
			}
			if _, err := ReadFile(path); err != nil {
				t.Errorf("tokens file no longer parses: %v", err)
			}

			if _, err := AppendBytes(before, tt.label, tt.entry); !errors.Is(err, ErrEntryUnencodable) {
				t.Errorf("AppendBytes err = %v, want ErrEntryUnencodable", err)
			}
		})
	}
}
