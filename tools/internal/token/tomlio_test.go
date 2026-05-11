package token

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFile(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantLabels []string
		wantErr    bool
	}{
		{
			name: "two entries",
			content: `[tokens.writer]
hash = "sha256-aaa"
paths = ["/docs/*"]
operations = ["publish"]

[tokens.reader]
hash = "sha256-bbb"
paths = ["/*"]
operations = ["read"]
`,
			wantLabels: []string{"reader", "writer"},
		},
		{
			name:       "empty file",
			content:    "",
			wantLabels: []string{},
		},
		{
			name:    "malformed toml",
			content: "this is not = valid [toml",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.toml")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("seed file: %v", err)
			}

			f, err := ReadFile(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ReadFile: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if f.Tokens == nil {
				t.Fatal("Tokens map is nil after ReadFile")
			}
			if len(f.Tokens) != len(tt.wantLabels) {
				t.Fatalf("got %d labels, want %d", len(f.Tokens), len(tt.wantLabels))
			}
			for _, label := range tt.wantLabels {
				if _, ok := f.Tokens[label]; !ok {
					t.Errorf("missing label %q", label)
				}
			}
		})
	}
}

func TestReadFileMissingIsNotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.toml")
	_, err := ReadFile(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadFile of missing file: err = %v, want errors.Is(os.ErrNotExist)", err)
	}
}

func TestAppendEntry(t *testing.T) {
	t.Run("creates file when missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")
		entry := Entry{
			Hash:       "sha256-deadbeef",
			Paths:      []string{"/docs/*"},
			Operations: []string{"publish"},
		}
		if err := AppendEntry(path, "writer", &entry); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read after append: %v", err)
		}
		s := string(data)
		for _, want := range []string{
			"[tokens.writer]",
			`hash = "sha256-deadbeef"`,
			`paths = ["/docs/*"]`,
			`operations = ["publish"]`,
		} {
			if !strings.Contains(s, want) {
				t.Errorf("output missing %q\ngot:\n%s", want, s)
			}
		}
	})

	t.Run("preserves existing content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")
		seed := "# existing comment, must survive\n[tokens.first]\nhash = \"sha256-aaa\"\npaths = [\"/*\"]\noperations = [\"read\"]\n"
		if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		entry := Entry{Hash: "sha256-bbb", Paths: []string{"/docs/*"}, Operations: []string{"publish"}}
		if err := AppendEntry(path, "second", &entry); err != nil {
			t.Fatalf("AppendEntry: %v", err)
		}
		data, _ := os.ReadFile(path)
		s := string(data)
		if !strings.Contains(s, "# existing comment, must survive") {
			t.Errorf("comment lost:\n%s", s)
		}
		if !strings.Contains(s, "[tokens.first]") || !strings.Contains(s, "[tokens.second]") {
			t.Errorf("entries missing:\n%s", s)
		}
	})

	t.Run("rejects duplicate label", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")
		entry := Entry{Hash: "sha256-aaa", Paths: []string{"/*"}, Operations: []string{"read"}}
		if err := AppendEntry(path, "writer", &entry); err != nil {
			t.Fatalf("first AppendEntry: %v", err)
		}
		err := AppendEntry(path, "writer", &entry)
		if !errors.Is(err, ErrLabelExists) {
			t.Fatalf("second AppendEntry: err = %v, want ErrLabelExists", err)
		}
	})
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.toml")
	file := File{Tokens: map[string]Entry{
		"writer": {Hash: "sha256-aaa", Paths: []string{"/docs/*"}, Operations: []string{"publish"}},
	}}
	if err := WriteFile(path, file); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Temp file must be gone after successful rename.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".tmp file not cleaned up: stat err = %v", err)
	}

	// Round-trip: read it back and confirm the entry.
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after WriteFile: %v", err)
	}
	if got.Tokens["writer"].Hash != "sha256-aaa" {
		t.Errorf("round-trip hash = %q, want sha256-aaa", got.Tokens["writer"].Hash)
	}
}

func TestFormatEntryShape(t *testing.T) {
	entry := Entry{
		Hash:       "sha256-deadbeef",
		Paths:      []string{"/docs/*", "/public/*"},
		Operations: []string{"read", "publish"},
	}
	got := FormatEntry("fritz-laptop", &entry)
	want := "\n[tokens.fritz-laptop]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n"
	if got != want {
		t.Errorf("FormatEntry mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
