package token

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestAppendBytesEmpty(t *testing.T) {
	got, err := AppendBytes(nil, "admin", &Entry{
		Hash:       "sha256-abc",
		Paths:      []string{"/"},
		Operations: []string{"read"},
	})
	if err != nil {
		t.Fatalf("AppendBytes: %v", err)
	}
	if !strings.Contains(string(got), "[tokens.admin]") {
		t.Errorf("output missing [tokens.admin]: %q", got)
	}
	var parsed File
	if err := toml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e, ok := parsed.Tokens["admin"]
	if !ok {
		t.Fatalf("admin missing after round-trip: %+v", parsed)
	}
	if e.Hash != "sha256-abc" {
		t.Errorf("Hash = %q", e.Hash)
	}
}

func TestAppendBytesPreservesExisting(t *testing.T) {
	existing := []byte("# operator note: do not edit\n\n[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	got, err := AppendBytes(existing, "user1", &Entry{
		Hash:       "sha256-bbb",
		Paths:      []string{"/team-a/*"},
		Operations: []string{"read", "publish"},
	})
	if err != nil {
		t.Fatalf("AppendBytes: %v", err)
	}
	if !strings.HasPrefix(string(got), string(existing)) {
		t.Errorf("existing prefix not preserved: %q", got)
	}
	if !strings.Contains(string(got), "[tokens.user1]") {
		t.Errorf("new entry not appended: %q", got)
	}
}

func TestAppendBytesLabelExists(t *testing.T) {
	existing := []byte("[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	_, err := AppendBytes(existing, "admin", &Entry{
		Hash:       "sha256-bbb",
		Paths:      []string{"/"},
		Operations: []string{"read"},
	})
	if !errors.Is(err, ErrLabelExists) {
		t.Errorf("err = %v, want ErrLabelExists", err)
	}
}

func TestAppendBytesNilEntry(t *testing.T) {
	if _, err := AppendBytes(nil, "x", nil); !errors.Is(err, ErrNilEntry) {
		t.Errorf("err = %v, want ErrNilEntry", err)
	}
}

func TestAppendBytesMalformed(t *testing.T) {
	if _, err := AppendBytes([]byte("not = ["), "x", &Entry{Hash: "h", Paths: []string{"/"}, Operations: []string{"read"}}); err == nil {
		t.Error("expected error on malformed existing TOML")
	}
}

func TestRemoveBytesPresent(t *testing.T) {
	existing := []byte("[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n\n[tokens.user1]\nhash = \"sha256-bbb\"\npaths = [\"/team-a/*\"]\noperations = [\"read\"]\n")
	got, err := RemoveBytes(existing, "user1")
	if err != nil {
		t.Fatalf("RemoveBytes: %v", err)
	}
	var parsed File
	if err := toml.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, has := parsed.Tokens["user1"]; has {
		t.Errorf("user1 still present after removal: %+v", parsed.Tokens)
	}
	if _, has := parsed.Tokens["admin"]; !has {
		t.Errorf("admin missing after removing user1: %+v", parsed.Tokens)
	}
}

func TestRemoveBytesAbsent(t *testing.T) {
	existing := []byte("[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	got, err := RemoveBytes(existing, "nope")
	if err != nil {
		t.Fatalf("RemoveBytes: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("expected unchanged bytes for absent label, got %q", got)
	}
}

func TestRemoveBytesEmpty(t *testing.T) {
	got, err := RemoveBytes(nil, "x")
	if err != nil {
		t.Fatalf("RemoveBytes: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %q", got)
	}
}

func TestParseBytesEmpty(t *testing.T) {
	f, err := ParseBytes(nil)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	// Tokens map must be non-nil so membership checks (`_, ok := f.Tokens[label]`)
	// don't need to special-case the empty input — symmetric with ReadFile.
	if f.Tokens == nil {
		t.Error("Tokens map nil on empty input")
	}
	if len(f.Tokens) != 0 {
		t.Errorf("Tokens populated on empty input: %+v", f.Tokens)
	}
}

func TestParseBytesPopulated(t *testing.T) {
	existing := []byte("[tokens.admin]\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n\n[tokens.user1]\nhash = \"sha256-bbb\"\npaths = [\"/team-a/*\"]\noperations = [\"read\", \"publish\"]\nexpires = \"2026-05-12T12:00:00Z\"\n")
	f, err := ParseBytes(existing)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(f.Tokens) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(f.Tokens), f.Tokens)
	}
	admin, ok := f.Tokens["admin"]
	if !ok {
		t.Fatalf("admin missing")
	}
	if admin.Hash != "sha256-aaa" {
		t.Errorf("admin.Hash = %q", admin.Hash)
	}
	user1, ok := f.Tokens["user1"]
	if !ok {
		t.Fatalf("user1 missing")
	}
	if user1.Expires != "2026-05-12T12:00:00Z" {
		t.Errorf("user1.Expires = %q", user1.Expires)
	}
}

func TestParseBytesQuotedLabel(t *testing.T) {
	// Labels that need quoting in TOML (dots, plus signs) must round-trip
	// through ParseBytes intact — the broker's drift sweep relies on a
	// labels-map lookup, not a substring search on the serialized form.
	existing := []byte(`[tokens."alice@example.com"]` + "\nhash = \"sha256-aaa\"\npaths = [\"/\"]\noperations = [\"read\"]\n")
	f, err := ParseBytes(existing)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if _, ok := f.Tokens["alice@example.com"]; !ok {
		t.Errorf("quoted label missing: %+v", f.Tokens)
	}
}

func TestParseBytesMalformed(t *testing.T) {
	if _, err := ParseBytes([]byte("not = [")); err == nil {
		t.Error("expected error on malformed TOML")
	}
}
