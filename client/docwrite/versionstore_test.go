package docwrite_test

import (
	"bytes"
	"io/fs"

	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// versionStore is the server's stored document model without a disk: each
// version is the bytes storefmt serializes, so tag normalization, the hash
// chain, the append merge and retention are the rules every backend shares.
type versionStore struct {
	docs map[string]map[int][]byte
}

func newVersionStore() *versionStore { return &versionStore{docs: map[string]map[int][]byte{}} }

// CurrentVersionResult is the newest version of path, 0 when there is none.
func (s *versionStore) CurrentVersionResult(path string) (int, error) {
	current := 0
	for version := range s.docs[path] {
		current = max(current, version)
	}
	return current, nil
}

// Get reads one retained version, the current one for 0.
func (s *versionStore) Get(path string, version int) (*storefmt.Document, error) {
	if version == 0 {
		version, _ = s.CurrentVersionResult(path) // never fails
	}
	stored, ok := s.docs[path][version]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &storefmt.Document{
		Content: storefmt.ExtractBody(stored), Version: version,
		Metadata: storefmt.ExtractMetadata(stored), ETag: storefmt.StoredETag(stored),
	}, nil
}

// WriteVersion stores the next version when expected is the current one.
func (s *versionStore) WriteVersion(path string, expected int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	if err := storefmt.ValidateWrite(content, meta); err != nil {
		return nil, err
	}
	current, _ := s.CurrentVersionResult(path) // never fails
	if expected >= 0 && expected != current {
		return &storefmt.Document{Version: current}, storefmt.ErrConflict
	}
	previous := s.docs[path][current]
	if current > 0 && bytes.Equal(storefmt.ExtractBody(previous), content) && storefmt.MetaEqual(storefmt.ExtractMetadata(previous), storefmt.NormalizeMetadata(meta)) {
		return &storefmt.Document{Version: current}, storefmt.ErrNotModified
	}
	stored, err := storefmt.SerializeVersion(&storefmt.VersionSpec{Path: path, Version: current + 1, Previous: previous, Content: content, Metadata: meta})
	if err != nil {
		return nil, err
	}
	if s.docs[path] == nil {
		s.docs[path] = map[int][]byte{}
	}
	s.docs[path][current+1] = stored
	if keep := storefmt.RetentionValue(meta); keep > 0 {
		for version := range s.docs[path] {
			if version <= current+1-keep {
				delete(s.docs[path], version)
			}
		}
	}
	return s.Get(path, current+1)
}

// Append joins content to the expected version under the merged metadata.
func (s *versionStore) Append(path string, expected int, content []byte, meta map[string]string) (*storefmt.Document, error) {
	base, err := s.Get(path, expected)
	if err != nil {
		if current, _ := s.CurrentVersionResult(path); current > 0 && current != expected { // never fails
			return &storefmt.Document{Version: current}, storefmt.ErrConflict
		}
		return nil, err
	}
	combined, err := storefmt.JoinContent(base.Content, content)
	if err != nil {
		return nil, err
	}
	return s.WriteVersion(path, expected, combined, storefmt.PrepareAppendMeta(path, base.Metadata, meta))
}
