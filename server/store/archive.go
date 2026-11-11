package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

const archiveStateName = "archive-state"

func readArchiveState(docDir string) (archived, exists bool, err error) {
	statePath := filepath.Join(docDir, archiveStateName)
	info, err := os.Lstat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("stat archive state: %w", err)
	}
	if !info.Mode().IsRegular() || (info.Size() != int64(len("true\n")) && info.Size() != int64(len("false\n"))) {
		return false, false, fmt.Errorf("%w: invalid archive state file", storefmt.ErrIntegrity)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		return false, false, fmt.Errorf("read archive state: %w", err)
	}
	switch string(data) {
	case "true\n":
		return true, true, nil
	case "false\n":
		return false, true, nil
	default:
		return false, false, fmt.Errorf("%w: invalid archive state %q", storefmt.ErrIntegrity, data)
	}
}

func effectiveArchived(docDir string, tipStored []byte) (bool, error) {
	archived, exists, err := readArchiveState(docDir)
	if err != nil {
		return false, err
	}
	if exists {
		return archived, nil
	}
	return storefmt.IsArchived(tipStored), nil
}

// writeArchiveState commits the archive flag through WriteFileAtomic.
func writeArchiveState(docDir string, archived bool) (committed bool, err error) {
	return WriteFileAtomic(filepath.Join(docDir, archiveStateName), 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, strconv.FormatBool(archived)+"\n")
		return err
	})
}

// ArchiveResult changes archive state and returns its committed result.
func (s *Store) ArchiveResult(reqPath string, archived bool) (*storefmt.Document, bool, error) {
	return s.ArchiveChecked(&storefmt.ArchiveSpec{Path: reqPath, Archived: archived})
}

// ArchiveChecked is ArchiveResult with an optional check run before the state
// changes; a no-op transition is decided first and never reaches it.
func (s *Store) ArchiveChecked(spec *storefmt.ArchiveSpec) (*storefmt.Document, bool, error) {
	reqPath, archived := spec.Path, spec.Archived
	if _, err := s.resolve(reqPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("resolve path: %w", err)
	}

	loc, err := s.locate(reqPath)
	if err != nil {
		return nil, false, err
	}
	currentVersion, err := s.currentVersion(&loc)
	if err != nil {
		return nil, false, err
	}
	if currentVersion == 0 {
		return nil, false, os.ErrNotExist
	}

	versionFile, err := s.resolve(loc.versionReqPath(currentVersion))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("resolve version file: %w", err)
	}

	data, err := os.ReadFile(versionFile)
	if err != nil {
		return nil, false, fmt.Errorf("read version file: %w", err)
	}
	info, err := os.Stat(versionFile)
	if err != nil {
		return nil, false, fmt.Errorf("stat version file: %w", err)
	}
	currentArchived, err := effectiveArchived(filepath.Dir(versionFile), data)
	if err != nil {
		return nil, false, fmt.Errorf("read current archive state: %w", err)
	}
	doc := documentFromStored(data, info.ModTime(), currentVersion, archived)
	if currentArchived == archived {
		if _, err := os.Stat(filepath.Join(filepath.Dir(versionFile), archiveStateName)); err == nil {
			if err := syncDir(filepath.Dir(versionFile)); err != nil {
				return doc, false, fmt.Errorf("%w: %w", storefmt.ErrCommittedNotSynced, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return doc, false, fmt.Errorf("stat archive state: %w", err)
		}
		s.updateArchiveIndex(reqPath, data, currentVersion, archived)
		return doc, false, nil
	}
	if spec.Check != nil {
		if err := spec.Check(storefmt.ArchiveChange{Path: storefmt.CanonicalPath(reqPath), Archived: archived}); err != nil {
			return nil, false, err
		}
	}

	committed, stateErr := writeArchiveState(filepath.Dir(versionFile), archived)
	if !committed {
		return nil, false, stateErr
	}

	s.updateArchiveIndex(reqPath, data, currentVersion, archived)

	if stateErr != nil {
		return doc, true, fmt.Errorf("%w: %w", storefmt.ErrCommittedNotSynced, stateErr)
	}
	return doc, true, nil
}

func (s *Store) updateArchiveIndex(reqPath string, stored []byte, version int, archived bool) {
	if archived {
		s.RemoveIndexEntry(reqPath)
		return
	}
	s.UpdateIndex(reqPath, storefmt.ExtractBody(stored), version)
}
