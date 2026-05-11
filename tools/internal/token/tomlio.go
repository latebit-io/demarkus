package token

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// File is the top-level shape of a tokens.toml — a map from label to Entry.
type File struct {
	Tokens map[string]Entry `toml:"tokens"`
}

// ErrLabelExists is returned by AppendEntry when the target file already
// contains a token with the given label. Callers should treat this as a
// definitive collision; minting must not silently overwrite.
var ErrLabelExists = errors.New("label already exists")

// ReadFile decodes a tokens.toml from disk. A non-existent file is reported
// as os.ErrNotExist via errors.Is; callers can treat that as an empty File.
// The Tokens map on the returned File is always non-nil.
func ReadFile(path string) (File, error) {
	var f File
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return File{}, fmt.Errorf("read tokens file %q: %w", path, err)
	}
	if f.Tokens == nil {
		f.Tokens = make(map[string]Entry)
	}
	return f, nil
}

// AppendEntry adds a new labeled entry to a tokens.toml file. If the file
// does not exist it is created. The append is text-style (matches the legacy
// CLI's on-disk format byte-for-byte), so any pre-existing comments and
// formatting in the file are preserved.
//
// Returns ErrLabelExists if the label is already present.
func AppendEntry(path, label string, entry *Entry) error {
	if existing, err := ReadFile(path); err == nil {
		if _, present := existing.Tokens[label]; present {
			return fmt.Errorf("%s: %w", label, ErrLabelExists)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	text := FormatEntry(label, entry)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open tokens file %q: %w", path, err)
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return fmt.Errorf("write tokens file %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close tokens file %q: %w", path, err)
	}
	return nil
}

// WriteFile writes a File to disk atomically (write to temp + rename).
// Existing on-disk formatting (comments, ordering, quoting style) is not
// preserved — this is the rewrite path used by revoke, not the append path.
func WriteFile(path string, file File) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp tokens file %q: %w", tmp, err)
	}
	if err := toml.NewEncoder(f).Encode(file); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encode tokens file %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp tokens file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %q to %q: %w", tmp, path, err)
	}
	return nil
}

// FormatEntry returns the TOML text representation of a single labeled
// entry, matching the on-disk byte shape the legacy demarkus-token CLI
// produced. Used by AppendEntry and also by the CLI when printing to stdout
// for the no-`-tokens` case.
func FormatEntry(label string, entry *Entry) string {
	return fmt.Sprintf("\n[tokens.%s]\nhash = %q\npaths = [%s]\noperations = [%s]\n",
		label,
		entry.Hash,
		quotedList(entry.Paths),
		quotedList(entry.Operations),
	)
}

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return strings.Join(quoted, ", ")
}
