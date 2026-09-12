package answerbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/latebit-io/demarkus/protocol/store"
)

// LoadStoreFixture reads an existing versioned corpus without renumbering history.
// Tasks and rubric live outside the served root.
func LoadStoreFixture(ctx context.Context, root, questions string) (Fixture, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Fixture{}, err
	}
	f := Fixture{StoreRoot: root, Hashes: make(map[string]string), storedVersions: make(map[string]map[int]bool)}
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	err = store.New(root).ExportDocs(ctx, func(docPath string, doc store.StoredDocument) error {
		if err := encoder.Encode(corpusEntry{Path: docPath, Document: doc}); err != nil {
			return err
		}
		f.VersionCount += len(doc.Versions)
		f.storedVersions[docPath] = make(map[int]bool, len(doc.Versions))
		for _, version := range doc.Versions {
			f.storedVersions[docPath][version.Version] = true
		}
		if doc.Archived {
			return nil
		}
		current := doc.Versions[len(doc.Versions)-1]
		f.Documents = append(f.Documents, Document{Path: docPath, Current: current.Version, Metadata: store.ExtractMetadata(current.Stored), Versions: []string{string(store.ExtractBody(current.Stored))}})
		return nil
	})
	if err != nil {
		return f, fmt.Errorf("inventory copied corpus: %w", err)
	}
	f.Hashes["corpus"] = fmt.Sprintf("sha256-%x", hash.Sum(nil))
	for _, item := range []struct {
		name   string
		target any
	}{{"tasks", &f.Tasks}, {"rubric", &f.Rubrics}} {
		raw, err := os.ReadFile(filepath.Join(questions, item.name+".json"))
		if err != nil {
			return f, err
		}
		if err := decodeJSON(raw, item.target); err != nil {
			return f, fmt.Errorf("decode %s: %w", item.name, err)
		}
		f.Hashes[item.name] = digest(raw)
	}
	err = f.Validate()
	return f, err
}

// InspectStore reports snapshot identity without sending any content to a model.
func InspectStore(ctx context.Context, root, questions string) (map[string]any, error) {
	f, err := LoadStoreFixture(ctx, root, questions)
	if err != nil {
		return nil, err
	}
	return map[string]any{"documents": len(f.Documents), "versions": f.VersionCount, "tasks": len(f.Tasks), "hashes": f.Hashes}, nil
}
