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
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read after append: %v", err)
		}
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
	t.Run("creates file when missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")
		file := File{Tokens: map[string]Entry{
			"writer": {Hash: "sha256-aaa", Paths: []string{"/docs/*"}, Operations: []string{"publish"}},
		}}
		if err := WriteFile(path, file); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".tmp file not cleaned up: stat err = %v", err)
		}
		got, err := ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile after WriteFile: %v", err)
		}
		if got.Tokens["writer"].Hash != "sha256-aaa" {
			t.Errorf("round-trip hash = %q, want sha256-aaa", got.Tokens["writer"].Hash)
		}
	})

	t.Run("overwrites existing file completely", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.toml")

		// Seed: file with one label.
		seed := File{Tokens: map[string]Entry{
			"old-writer": {Hash: "sha256-old", Paths: []string{"/old/*"}, Operations: []string{"publish"}},
		}}
		if err := WriteFile(path, seed); err != nil {
			t.Fatalf("WriteFile seed: %v", err)
		}

		// Overwrite: a completely different file.
		replacement := File{Tokens: map[string]Entry{
			"new-writer": {Hash: "sha256-new", Paths: []string{"/new/*"}, Operations: []string{"read"}},
		}}
		if err := WriteFile(path, replacement); err != nil {
			t.Fatalf("WriteFile overwrite: %v", err)
		}
		if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf(".tmp file not cleaned up after overwrite: stat err = %v", err)
		}

		got, err := ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile after overwrite: %v", err)
		}
		if _, present := got.Tokens["old-writer"]; present {
			t.Error("old label survived overwrite; WriteFile must replace, not merge")
		}
		stored, ok := got.Tokens["new-writer"]
		if !ok {
			t.Fatal("new label missing after overwrite")
		}
		if stored.Hash != "sha256-new" {
			t.Errorf("new hash = %q, want sha256-new", stored.Hash)
		}
		if len(stored.Paths) != 1 || stored.Paths[0] != "/new/*" {
			t.Errorf("new paths = %v, want [/new/*]", stored.Paths)
		}
		if len(stored.Operations) != 1 || stored.Operations[0] != "read" {
			t.Errorf("new operations = %v, want [read]", stored.Operations)
		}
	})
}

func TestFormatEntryShape(t *testing.T) {
	entry := Entry{
		Hash:       "sha256-deadbeef",
		Paths:      []string{"/docs/*", "/public/*"},
		Operations: []string{"read", "publish"},
	}
	tests := []struct {
		name  string
		label string
		want  string
	}{
		{
			name:  "bare key — alphanumeric with hyphen",
			label: "fritz-laptop",
			want:  "\n[tokens.fritz-laptop]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n",
		},
		{
			name:  "bare key — underscore allowed",
			label: "team_a_2026",
			want:  "\n[tokens.team_a_2026]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n",
		},
		{
			name:  "quoted key — contains dot",
			label: "fritz.laptop",
			want:  "\n[tokens.\"fritz.laptop\"]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n",
		},
		{
			name:  "quoted key — contains whitespace",
			label: "team a",
			want:  "\n[tokens.\"team a\"]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n",
		},
		{
			name:  "quoted key — contains @ for email-style labels",
			label: "fritz@example.com",
			want:  "\n[tokens.\"fritz@example.com\"]\nhash = \"sha256-deadbeef\"\npaths = [\"/docs/*\", \"/public/*\"]\noperations = [\"read\", \"publish\"]\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatEntry(tt.label, &entry)
			if got != tt.want {
				t.Errorf("FormatEntry(%q):\ngot:\n%s\nwant:\n%s", tt.label, got, tt.want)
			}
		})
	}
}

// TestFormatEntryExpires verifies the `expires` line is emitted when
// Entry.Expires is set, and omitted otherwise (matching `omitempty` on the
// struct tag). The broker depends on this — short-lived tokens with a
// non-empty Expires field would otherwise be silently written as
// never-expiring entries.
func TestFormatEntryExpires(t *testing.T) {
	t.Run("emitted when non-empty", func(t *testing.T) {
		entry := Entry{
			Hash:       "sha256-aaa",
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
			Expires:    "2026-05-12T15:04:05Z",
		}
		got := FormatEntry("writer", &entry)
		want := "\n[tokens.writer]\nhash = \"sha256-aaa\"\npaths = [\"/*\"]\noperations = [\"publish\"]\nexpires = \"2026-05-12T15:04:05Z\"\n"
		if got != want {
			t.Errorf("FormatEntry with Expires:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("omitted when empty", func(t *testing.T) {
		entry := Entry{
			Hash:       "sha256-aaa",
			Paths:      []string{"/*"},
			Operations: []string{"publish"},
		}
		got := FormatEntry("writer", &entry)
		if strings.Contains(got, "expires") {
			t.Errorf("FormatEntry emitted expires line for empty value:\n%s", got)
		}
	})
}

// TestAppendEntryExpiresRoundTrip verifies an Expires value survives
// AppendEntry → ReadFile. This is the broker's primary write path; a
// regression here would silently produce never-expiring tokens.
func TestAppendEntryExpiresRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.toml")
	entry := Entry{
		Hash:       "sha256-aaa",
		Paths:      []string{"/team/*"},
		Operations: []string{"publish"},
		Expires:    "2026-05-12T15:04:05Z",
	}
	if err := AppendEntry(path, "broker-issued", &entry); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	stored, ok := got.Tokens["broker-issued"]
	if !ok {
		t.Fatal("label missing after round-trip")
	}
	if stored.Expires != entry.Expires {
		t.Errorf("Expires lost in round-trip: got %q, want %q", stored.Expires, entry.Expires)
	}
}

// TestAppendEntryAtomic verifies that AppendEntry publishes via temp+rename
// rather than in-place appending: the `.tmp` sidecar must not survive a
// successful call, and the destination file must still contain both the
// pre-existing entry and the new one. This protects against the regression
// where in-place O_APPEND let concurrent readers observe a half-written
// stanza.
func TestAppendEntryAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.toml")
	seed := "# existing comment, must survive\n[tokens.first]\nhash = \"sha256-aaa\"\npaths = [\"/*\"]\noperations = [\"read\"]\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	entry := Entry{Hash: "sha256-bbb", Paths: []string{"/docs/*"}, Operations: []string{"publish"}}
	if err := AppendEntry(path, "second", &entry); err != nil {
		t.Fatalf("AppendEntry: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".tmp sidecar still present after AppendEntry success: stat err = %v", err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after AppendEntry: %v", err)
	}
	for _, label := range []string{"first", "second"} {
		if _, ok := got.Tokens[label]; !ok {
			t.Errorf("label %q missing after atomic publish", label)
		}
	}
}

// TestAppendEntryNil verifies AppendEntry rejects a nil entry with
// ErrNilEntry rather than panicking.
func TestAppendEntryNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.toml")
	if err := AppendEntry(path, "writer", nil); !errors.Is(err, ErrNilEntry) {
		t.Errorf("AppendEntry(nil): err = %v, want ErrNilEntry", err)
	}
	// The file must not be created as a side effect of the nil-entry call.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tokens file should not exist after nil-entry rejection: stat err = %v", err)
	}
}

// TestFormatEntryQuotedKeyRoundTrip verifies that a label written with a
// quoted TOML key reads back as the same map key — protecting against the
// regression where unquoted dotted labels parsed as nested tables.
func TestFormatEntryQuotedKeyRoundTrip(t *testing.T) {
	tricky := []string{"fritz.laptop", "team a", "fritz@example.com"}
	for _, label := range tricky {
		t.Run(label, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tokens.toml")
			entry := Entry{
				Hash:       "sha256-aaa",
				Paths:      []string{"/*"},
				Operations: []string{"publish"},
			}
			if err := AppendEntry(path, label, &entry); err != nil {
				t.Fatalf("AppendEntry: %v", err)
			}
			got, err := ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if _, ok := got.Tokens[label]; !ok {
				t.Errorf("round-trip lost label %q; got keys: %v", label, mapKeys(got.Tokens))
			}
		})
	}
}

func mapKeys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
