package token

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// ErrNilEntry is returned by AppendEntry when the provided entry pointer
// is nil. Returning rather than panicking lets the broker recover from a
// programming error without crashing its issuance loop.
var ErrNilEntry = errors.New("entry is required")

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
// does not exist it is created. Existing comments and formatting in the
// file are preserved byte-for-byte — the append works by reading the raw
// file bytes, concatenating FormatEntry(...), and publishing the result
// via an atomic temp+rename. Readers see either the pre-append file or
// the post-append file, never a half-written stanza.
//
// The read-modify-write sequence is serialized across processes by an
// advisory flock(2) on a sidecar `<path>.lock` file. Concurrent invocations
// from the same host are safe; concurrent writers across NFS or other
// filesystems without working flock are not.
//
// Returns ErrLabelExists if the label is already present, or ErrNilEntry
// if entry is nil.
func AppendEntry(path, label string, entry *Entry) error {
	if entry == nil {
		return ErrNilEntry
	}
	return withLockedFile(path, func() error {
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read tokens file %q: %w", path, err)
		}
		if len(existing) > 0 {
			var current File
			if err := toml.Unmarshal(existing, &current); err != nil {
				return fmt.Errorf("decode tokens file %q: %w", path, err)
			}
			if _, present := current.Tokens[label]; present {
				return fmt.Errorf("%s: %w", label, ErrLabelExists)
			}
		}
		return writeAtomic(path, func(w io.Writer) error {
			if len(existing) > 0 {
				if _, err := w.Write(existing); err != nil {
					return fmt.Errorf("copy existing tokens file content: %w", err)
				}
			}
			if _, err := io.WriteString(w, FormatEntry(label, entry)); err != nil {
				return fmt.Errorf("write new token entry: %w", err)
			}
			return nil
		})
	})
}

// ParseBytes decodes tokens.toml bytes into a File. An empty input
// returns an empty File with Tokens initialized to a non-nil map, the
// same convention ReadFile uses. The broker calls this to inspect a
// world's tokens Secret directly — drift detection in the expiry
// sweeper compares broker-side issuance labels against the labels
// actually present in the world's TOML, and a quoted-key parse here is
// safer than a substring search on the serialized bytes.
func ParseBytes(existing []byte) (File, error) {
	f := File{Tokens: make(map[string]Entry)}
	if len(existing) == 0 {
		return f, nil
	}
	if err := toml.Unmarshal(existing, &f); err != nil {
		return File{}, fmt.Errorf("decode tokens bytes: %w", err)
	}
	if f.Tokens == nil {
		f.Tokens = make(map[string]Entry)
	}
	return f, nil
}

// AppendBytes is the in-memory analogue of AppendEntry: given existing
// tokens.toml bytes (possibly empty), it returns the bytes with a new
// labeled entry appended. Used by the demarkus-broker, which writes to a
// Kubernetes Secret rather than a file and so has no use for the flock +
// atomic-publish machinery AppendEntry layers on top.
//
// Returns ErrLabelExists if the label is already present in existing,
// ErrNilEntry if entry is nil. The result is byte-equivalent to
// existing + FormatEntry(label, entry) — comments and prior ordering in
// existing are preserved.
func AppendBytes(existing []byte, label string, entry *Entry) ([]byte, error) {
	if entry == nil {
		return nil, ErrNilEntry
	}
	if len(existing) > 0 {
		var current File
		if err := toml.Unmarshal(existing, &current); err != nil {
			return nil, fmt.Errorf("decode tokens bytes: %w", err)
		}
		if _, present := current.Tokens[label]; present {
			return nil, fmt.Errorf("%s: %w", label, ErrLabelExists)
		}
	}
	formatted := FormatEntry(label, entry)
	out := make([]byte, 0, len(existing)+len(formatted))
	out = append(out, existing...)
	out = append(out, formatted...)
	return out, nil
}

// RemoveBytes returns tokens.toml bytes with the given label removed. If
// the label is not present in existing, the original bytes are returned
// unchanged with a nil error — revoke-of-missing is not an error condition,
// matching the broker's expiry-sweeper and idempotent-DELETE semantics.
//
// Re-encoding does not preserve original formatting (comments, ordering);
// this is the rewrite path the broker uses on the Kubernetes Secret, the
// in-memory analogue of WriteFile, not AppendEntry.
func RemoveBytes(existing []byte, label string) ([]byte, error) {
	if len(existing) == 0 {
		return existing, nil
	}
	var current File
	if err := toml.Unmarshal(existing, &current); err != nil {
		return nil, fmt.Errorf("decode tokens bytes: %w", err)
	}
	if _, present := current.Tokens[label]; !present {
		return existing, nil
	}
	delete(current.Tokens, label)
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(current); err != nil {
		return nil, fmt.Errorf("encode tokens bytes: %w", err)
	}
	return []byte(buf.String()), nil
}

// WriteFile writes a File to disk atomically (write to temp + rename) under
// the same advisory flock as AppendEntry, so revoke and generate cannot
// interleave across processes. Existing on-disk formatting (comments,
// ordering, quoting style) is not preserved — this is the rewrite path used
// by revoke, not the append path.
func WriteFile(path string, file File) error {
	return withLockedFile(path, func() error {
		return writeAtomic(path, func(w io.Writer) error {
			if err := toml.NewEncoder(w).Encode(file); err != nil {
				return fmt.Errorf("encode tokens file: %w", err)
			}
			return nil
		})
	})
}

// writeAtomic publishes a file at path by writing to a sibling temp file
// and renaming it into place. Callers supply fn, which receives an
// io.Writer pointing at the temp file. The temp file is removed on any
// failure path; on success only the destination remains.
//
// The publish is both atomic (concurrent readers see either the old file
// or the new one, never a partial write) and durable (an fsync(2) on the
// data file before close plus an fsync(2) on the containing directory
// after rename ensures the update survives an OS-level crash immediately
// after the call returns). Both are properties operators expect from
// `demarkus-token revoke` and the broker's issuance loop.
//
// Callers are expected to hold the appropriate advisory lock via
// withLockedFile. writeAtomic itself does not lock — locking and
// atomic-publish are separate concerns combined at the call sites.
func writeAtomic(path string, fn func(io.Writer) error) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp tokens file %q: %w", tmp, err)
	}
	if err := fn(f); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync temp tokens file %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp tokens file %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %q to %q: %w", tmp, path, err)
	}
	// fsync the containing directory so the rename's directory-entry
	// update is durable. Without this, an OS crash immediately after
	// Rename returns can leave the destination missing or pointing at
	// the old inode after recovery — fatal for revoke confirmations.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open parent dir of %q for fsync: %w", path, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("fsync parent dir of %q: %w", path, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close parent dir of %q: %w", path, err)
	}
	return nil
}

// withLockedFile holds an exclusive advisory lock on a sidecar `<path>.lock`
// while fn runs, serializing AppendEntry/WriteFile across processes on the
// same host. The sidecar is preferred over locking the data file directly
// so the data file is never created as a side effect of acquiring the lock.
func withLockedFile(path string, fn func() error) error {
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %q: %w", lockPath, err)
	}
	defer func() { _ = lf.Close() }()
	if err := lockExclusive(lf); err != nil {
		return fmt.Errorf("acquire lock on %q: %w", lockPath, err)
	}
	return fn()
}

// FormatEntry returns the TOML text representation of a single labeled
// entry. Labels that satisfy TOML's bare-key grammar (alphanumeric, `_`,
// `-`) are emitted unquoted — the common case for human-chosen labels and
// the format the legacy demarkus-token CLI produces. Labels with dots,
// whitespace, or other characters are emitted as quoted keys, e.g.
// `[tokens."with.dot"]`, which is otherwise valid TOML and round-trips
// through ReadFile to the same map key.
//
// Entry.Expires is emitted only when non-empty, matching the `omitempty`
// semantics of the struct tag. Callers must pass a non-nil entry; panics
// on nil are a programmer error and the call site in AppendEntry guards
// against that path.
func FormatEntry(label string, entry *Entry) string {
	keyExpr := label
	if !isBareTOMLKey(label) {
		keyExpr = strconv.Quote(label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n[tokens.%s]\nhash = %q\npaths = [%s]\noperations = [%s]\n",
		keyExpr,
		entry.Hash,
		quotedList(entry.Paths),
		quotedList(entry.Operations),
	)
	if entry.Expires != "" {
		fmt.Fprintf(&b, "expires = %q\n", entry.Expires)
	}
	return b.String()
}

// isBareTOMLKey reports whether s is a valid TOML bare key — the unquoted
// key syntax that accepts only alphanumerics, underscore, and hyphen.
// Empty strings are rejected.
func isBareTOMLKey(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return strings.Join(quoted, ", ")
}
