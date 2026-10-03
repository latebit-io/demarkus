package bucketstore

import (
	"bytes"
	"fmt"
	"maps"
	"strings"
	"testing"
)

const (
	testWorldID  = "52b471f7-8d38-4c89-b44a-6f4f8b1a4f48"
	otherWorldID = "00000000-0000-0000-8000-000000000001"
	testModified = "2026-08-22T12:34:56Z"
)

func TestModelIdentifiers(t *testing.T) {
	t.Run("world IDs", func(t *testing.T) {
		tests := []struct {
			name  string
			value string
			valid bool
		}{
			{name: "canonical", value: testWorldID, valid: true},
			{name: "version agnostic", value: "52b471f7-8d38-0c89-b44a-6f4f8b1a4f48", valid: true},
			{name: "nil UUID", value: "00000000-0000-0000-0000-000000000000"},
			{name: "non-RFC variant", value: "52b471f7-8d38-4c89-f44a-6f4f8b1a4f48"},
			{name: "unhyphenated", value: "52b471f78d384c89b44a6f4f8b1a4f48"},
			{name: "uppercase", value: "52B471F7-8D38-4C89-B44A-6F4F8B1A4F48"},
			{name: "bad separator", value: "52b471f7_8d38-4c89-b44a-6f4f8b1a4f48"},
			{name: "non-hex", value: "52b471f7-8d38-4c89-b44a-6f4f8b1a4f4g"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if got := validUUID(test.value); got != test.valid {
					t.Errorf("validUUID(%q) = %v, want %v", test.value, got, test.valid)
				}
			})
		}
	})

	t.Run("hashes", func(t *testing.T) {
		tests := []struct {
			name  string
			value string
			valid bool
		}{
			{name: "lowercase", value: strings.Repeat("a", 64), valid: true},
			{name: "digits", value: strings.Repeat("0", 64), valid: true},
			{name: "short", value: strings.Repeat("a", 63)},
			{name: "uppercase", value: strings.Repeat("A", 64)},
			{name: "prefixed", value: "sha256-" + strings.Repeat("a", 64)},
			{name: "non-hex", value: strings.Repeat("g", 64)},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if got := validHash(test.value); got != test.valid {
					t.Errorf("validHash(%q) = %v, want %v", test.value, got, test.valid)
				}
			})
		}
	})

	t.Run("keys", func(t *testing.T) {
		hash := strings.Repeat("a", 64)
		tests := []struct {
			name string
			got  string
			want string
		}{
			{name: "head", got: markerKey, want: "_demarkus/v1/head.json"},
			{name: "blob", got: blobKey(hash), want: "_demarkus/v1/blobs/" + hash},
			{name: "history", got: historyKey(hash), want: "_demarkus/v1/history/" + hash + ".json"},
			{name: "shard", got: shardKey("af", hash), want: "_demarkus/v1/index/af/" + hash + ".json"},
			{name: "root", got: rootKey(hash), want: "_demarkus/v1/roots/" + hash + ".json"},
			{name: "segment", got: segmentKey(segmentRef{label: segmentLabel(0xaf, 12), window: 0x1f}, 2), want: "_demarkus/v1/segments/12-0af/000000000000001f/2.json"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if test.got != test.want {
					t.Errorf("key = %q, want %q", test.got, test.want)
				}
			})
		}
	})
}

func TestModelTimestampsAndImportance(t *testing.T) {
	t.Run("timestamps", func(t *testing.T) {
		tests := []struct {
			name  string
			value string
			valid bool
		}{
			{name: "UTC seconds", value: testModified, valid: true},
			{name: "fraction", value: "2026-08-22T12:34:56.1Z"},
			{name: "zero offset", value: "2026-08-22T12:34:56+00:00"},
			{name: "non-UTC", value: "2026-08-22T13:34:56+01:00"},
			{name: "missing seconds", value: "2026-08-22T12:34Z"},
			{name: "invalid date", value: "2026-02-30T12:34:56Z"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if _, err := parseTimestamp(test.value); (err == nil) != test.valid {
					t.Errorf("parseTimestamp(%q) = %v, want valid %v", test.value, err, test.valid)
				}
			})
		}
	})

	t.Run("importance", func(t *testing.T) {
		tests := []struct {
			name  string
			value string
			valid bool
		}{
			{name: "zero", value: "0", valid: true},
			{name: "one", value: "1", valid: true},
			{name: "decimal", value: "0.75", valid: true},
			{name: "small decimal", value: "0.000001", valid: true},
			{name: "trailing zero", value: "0.50"},
			{name: "leading zero", value: "00.5"},
			{name: "exponent", value: "5e-1"},
			{name: "negative zero", value: "-0"},
			{name: "above one", value: "1.1"},
			{name: "empty", value: ""},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, err := parseCanonicalImportance(test.value)
				if (err == nil) != test.valid {
					t.Errorf("parseCanonicalImportance(%q) error = %v, valid = %v", test.value, err, test.valid)
				}
			})
		}
	})
}

func TestCanonicalJSON(t *testing.T) {
	rootHash := strings.Repeat("a", 64)
	checkpoint := checkpointObject{
		Schema:   logSchema,
		WorldID:  testWorldID,
		Sequence: 1,
		Root:     objectRef{Key: rootKey(rootHash), Hash: rootHash},
	}
	canonical, err := marshalImmutable(checkpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	want := `{"schema":1,"world_id":"52b471f7-8d38-4c89-b44a-6f4f8b1a4f48","sequence":1,"root":{"key":"_demarkus/v1/roots/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"tip":""}`
	reordered := `{"world_id":"52b471f7-8d38-4c89-b44a-6f4f8b1a4f48","schema":1,"sequence":1,"root":{"key":"_demarkus/v1/roots/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"tip":""}`
	if string(canonical) != want {
		t.Fatalf("canonical checkpoint:\n%s\nwant:\n%s", canonical, want)
	}

	t.Run("decode", func(t *testing.T) {
		tests := []struct {
			name    string
			data    []byte
			wantErr bool
		}{
			{name: "canonical", data: canonical},
			{name: "leading whitespace", data: append([]byte(" "), canonical...), wantErr: true},
			{name: "trailing newline", data: append(bytes.Clone(canonical), '\n'), wantErr: true},
			{name: "trailing value", data: append(bytes.Clone(canonical), []byte(`{}`)...), wantErr: true},
			{name: "unknown field", data: []byte(strings.TrimSuffix(want, "}") + `,"extra":true}`), wantErr: true},
			{name: "field order", data: []byte(reordered), wantErr: true},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				var decoded checkpointObject
				err := decodeImmutable(test.data, &decoded)
				if (err != nil) != test.wantErr {
					t.Errorf("decodeImmutable() error = %v, wantErr %v", err, test.wantErr)
				}
			})
		}
	})

	t.Run("map keys", func(t *testing.T) {
		record := catalogRecord{
			Path:       "/a.md",
			Title:      "A",
			Tags:       make([]string, 0),
			Importance: "0.5",
			Modified:   testModified,
			Metadata:   map[string]string{"z": "last", "a": "first"},
		}
		data, err := marshalImmutable(record)
		if err != nil {
			t.Fatalf("marshal catalog: %v", err)
		}
		if !bytes.Contains(data, []byte(`"metadata":{"a":"first","z":"last"}`)) {
			t.Errorf("metadata keys are not sorted: %s", data)
		}
	})

	t.Run("null collection", func(t *testing.T) {
		slot := validWriteSlot()
		slot.Entries = nil
		data, err := marshalImmutable(slot)
		if err != nil {
			t.Fatal(err)
		}
		var decoded slotObject
		if err := decodeImmutable(data, &decoded); err != nil {
			t.Fatalf("decode canonical null: %v", err)
		}
		if err := validateSlot(&decoded, decoded.First); err == nil {
			t.Fatal("validateSlot() accepted null entries")
		}
	})
}

func TestGoldenHistoryObject(t *testing.T) {
	pathSum := strings.Repeat("1", 64)
	blobSum := strings.Repeat("2", 64)
	bodySum := strings.Repeat("3", 64)
	history := historyObject{
		Schema:   historySchema,
		PathHash: pathSum,
		First:    257,
		Last:     257,
		Entries: []historyEntry{{
			Version:  257,
			Blob:     objectRef{Key: blobKey(blobSum), Hash: blobSum},
			BodyHash: "sha256-" + bodySum,
			Modified: testModified,
		}},
	}
	want := `{"schema":1,"path_hash":"1111111111111111111111111111111111111111111111111111111111111111","first":257,"last":257,"entries":[{"version":257,"blob":{"key":"_demarkus/v1/blobs/2222222222222222222222222222222222222222222222222222222222222222","hash":"2222222222222222222222222222222222222222222222222222222222222222"},"body_hash":"sha256-3333333333333333333333333333333333333333333333333333333333333333","modified":"2026-08-22T12:34:56Z"}]}`
	const wantHash = "5e05909fff72d37f900610461db9d4b779fe075d2e68239c7cb73a1f31b12c02"

	object, ref, err := immutableJSON(historyKey, history)
	if err != nil {
		t.Fatalf("build history: %v", err)
	}
	if string(object.Data) != want {
		t.Fatalf("history JSON:\n%s\nwant:\n%s", object.Data, want)
	}
	if ref.Hash != wantHash {
		t.Fatalf("history hash = %q, want %q", ref.Hash, wantHash)
	}
	if ref.Key != historyKey(wantHash) || object.Key != ref.Key {
		t.Errorf("history keys = object %q ref %q, want %q", object.Key, ref.Key, historyKey(wantHash))
	}
	if err := validateHistoryObject(&history); err != nil {
		t.Errorf("validate golden history: %v", err)
	}
}

func TestModelCollectionValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	validCatalog := catalogRecord{
		Path:       "/a.md",
		Title:      "A",
		Tags:       make([]string, 0),
		Importance: "0.5",
		Modified:   testModified,
		Metadata:   make(map[string]string),
	}
	tests := []struct {
		name     string
		validate func() error
	}{
		{name: "checkpoint root", validate: func() error {
			return validateCheckpoint(&checkpointObject{Schema: logSchema, WorldID: testWorldID, Sequence: 1}, 1)
		}},
		{name: "root shards", validate: func() error {
			return validateFoldedRoot(&foldedRoot{Schema: foldedSchema, WorldID: testWorldID}, testWorldID)
		}},
		{name: "shard entries", validate: func() error {
			return validateFoldedShard(&foldedShard{Schema: foldedSchema, Shard: "0"}, 0, 0)
		}},
		{name: "history entries", validate: func() error {
			return validateHistoryObject(&historyObject{Schema: historySchema, PathHash: hash, First: 1, Last: 1})
		}},
		{name: "entry history", validate: func() error {
			entry := testEntry("/a.md", false, "")
			entry.History = nil
			return validateFoldedEntry(&entry, 0, 0)
		}},
		{name: "catalog tags", validate: func() error {
			record := validCatalog
			record.Tags = nil
			return validateCatalogRecord(&record, record.Path, record.Modified)
		}},
		{name: "catalog metadata", validate: func() error {
			record := validCatalog
			record.Metadata = nil
			return validateCatalogRecord(&record, record.Path, record.Modified)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); err == nil {
				t.Error("validator accepted nil collection")
			}
		})
	}
}

// validWriteSlot is a slot holding one valid write of /a.md at version 1.
func validWriteSlot() slotObject {
	blobHash, bodyHash := strings.Repeat("b", 64), "sha256-"+strings.Repeat("c", 64)
	record := catalogRecord{Path: "/a.md", Title: "A", Tags: []string{}, Importance: "0.5", Modified: testModified, Metadata: map[string]string{}}
	return slotObject{
		Schema: logSchema, WorldID: testWorldID, First: 2, Store: otherWorldID,
		Entries: []slotEntry{{
			OperationID: "00000000-0000-4000-8000-000000000002", Op: "publish", Path: "/a.md",
			Current: 1, First: 1, BodyHash: bodyHash, Modified: testModified, Catalog: &record,
			Version: &historyEntry{Version: 1, Blob: objectRef{Key: blobKey(blobHash), Hash: blobHash}, BodyHash: bodyHash, Modified: testModified},
		}},
	}
}

func TestSlotValidation(t *testing.T) {
	archive := func(slot *slotObject) {
		entry := &slot.Entries[0]
		entry.Op, entry.Archived, entry.Catalog, entry.Version = "archive", true, nil, nil
	}
	tests := []struct {
		name    string
		mutate  func(*slotObject)
		wantErr bool
	}{
		{name: "write", mutate: func(*slotObject) {}},
		{name: "archive transition", mutate: archive},
		{name: "unarchive transition", mutate: func(slot *slotObject) {
			archive(slot)
			slot.Entries[0].Op, slot.Entries[0].Archived = "publish", false
		}},
		{name: "predecessor", mutate: func(slot *slotObject) { slot.Prev = strings.Repeat("d", 64) }},
		{name: "key names another sequence", mutate: func(slot *slotObject) { slot.First = 3 }, wantErr: true},
		{name: "genesis sequence", mutate: func(slot *slotObject) { slot.First = 1 }, wantErr: true},
		{name: "no entries", mutate: func(slot *slotObject) { slot.Entries = []slotEntry{} }, wantErr: true},
		{name: "too many entries", mutate: func(slot *slotObject) {
			for len(slot.Entries) <= maxSlotEntries {
				entry := slot.Entries[0]
				entry.OperationID = fmt.Sprintf("00000000-0000-4000-8000-%012x", len(slot.Entries)+10)
				slot.Entries = append(slot.Entries, entry)
			}
		}, wantErr: true},
		{name: "duplicate operation", mutate: func(slot *slotObject) {
			slot.Entries = append(slot.Entries, slot.Entries[0])
		}, wantErr: true},
		{name: "bad store ID", mutate: func(slot *slotObject) { slot.Store = "replica-1" }, wantErr: true},
		{name: "bad predecessor", mutate: func(slot *slotObject) { slot.Prev = "nope" }, wantErr: true},
		{name: "first past current", mutate: func(slot *slotObject) { slot.Entries[0].First = 2 }, wantErr: true},
		{name: "write without version", mutate: func(slot *slotObject) {
			slot.Entries[0].Version = nil
		}, wantErr: true},
		{name: "write left archived", mutate: func(slot *slotObject) { slot.Entries[0].Archived = true }, wantErr: true},
		{name: "version from another write", mutate: func(slot *slotObject) {
			slot.Entries[0].Version.Version = 2
		}, wantErr: true},
		{name: "archive op that unarchives", mutate: func(slot *slotObject) {
			archive(slot)
			slot.Entries[0].Archived = false
		}, wantErr: true},
		{name: "archive with a catalog", mutate: func(slot *slotObject) {
			record := *validWriteSlot().Entries[0].Catalog
			archive(slot)
			slot.Entries[0].Catalog = &record
		}, wantErr: true},
		{name: "unknown op", mutate: func(slot *slotObject) { slot.Entries[0].Op = "delete" }, wantErr: true},
		{name: "directory path", mutate: func(slot *slotObject) { slot.Entries[0].Path = "/" }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			slot := validWriteSlot()
			test.mutate(&slot)
			err := validateSlot(&slot, 2)
			if (err != nil) != test.wantErr {
				t.Errorf("validateSlot() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestHistoryBlockValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	ref := func(first, last int) blockRef { return blockRef{First: first, Last: last, Hash: hash} }
	tests := []struct {
		name    string
		blocks  []blockRef
		current int
		wantErr bool
	}{
		{name: "absolute contiguous blocks", blocks: []blockRef{ref(100, 256), ref(257, 300)}, current: 300},
		{name: "range crosses block", blocks: []blockRef{ref(256, 257)}, current: 257, wantErr: true},
		{name: "gap", blocks: []blockRef{ref(100, 250), ref(257, 300)}, current: 300, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateBlocks(test.blocks, test.current)
			if (err != nil) != test.wantErr {
				t.Errorf("validateBlocks() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCatalogSelfConsistency(t *testing.T) {
	base := catalogRecord{
		Path:       "/docs/a.md",
		Title:      "Declared",
		Tags:       []string{"go", "storage"},
		Importance: "0.8",
		Modified:   testModified,
		Metadata: map[string]string{
			"title":      " Declared ",
			"tags":       "go, storage",
			"importance": " 0.80 ",
		},
	}
	tests := []struct {
		name    string
		mutate  func(*catalogRecord)
		wantErr bool
	}{
		{name: "consistent"},
		{name: "body-derived spaced title", mutate: func(record *catalogRecord) {
			delete(record.Metadata, "title")
			record.Title = " spaced.md"
		}},
		{name: "noncanonical record importance", mutate: func(record *catalogRecord) { record.Importance = "0.80" }, wantErr: true},
		{name: "tag mismatch", mutate: func(record *catalogRecord) { record.Tags = []string{"go"} }, wantErr: true},
		{name: "title mismatch", mutate: func(record *catalogRecord) { record.Title = "Other" }, wantErr: true},
		{name: "modified mismatch", mutate: func(record *catalogRecord) { record.Modified = "2026-08-22T12:34:55Z" }, wantErr: true},
		{name: "reserved metadata", mutate: func(record *catalogRecord) { record.Metadata["version"] = "1" }, wantErr: true},
		{name: "metadata newline", mutate: func(record *catalogRecord) { record.Metadata["project"] = "bad\nvalue" }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := base
			record.Tags = append([]string(nil), base.Tags...)
			record.Metadata = map[string]string{}
			maps.Copy(record.Metadata, base.Metadata)
			if test.mutate != nil {
				test.mutate(&record)
			}
			err := validateCatalogRecord(&record, base.Path, base.Modified)
			if (err != nil) != test.wantErr {
				t.Errorf("validateCatalogRecord() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestFoldedEntryRejectsUnaddressablePaths(t *testing.T) {
	paths := []string{
		"/line\nbreak.md",
		"/" + strings.Repeat("a", 4090) + ".md",
	}
	for _, path := range paths {
		t.Run(fmt.Sprintf("%d-bytes", len(path)), func(t *testing.T) {
			entry := testEntry(path, false, "")
			if err := validateFoldedEntry(&entry, 0, 0); err == nil {
				t.Errorf("validateFoldedEntry() accepted %q", path)
			}
		})
	}
}

func TestFoldedEntryAllowsPathAgnosticDocuments(t *testing.T) {
	entry := testEntry("/x", false, "")
	if err := validateFoldedEntry(&entry, 0, 0); err != nil {
		t.Errorf("validateFoldedEntry(/x): %v", err)
	}
}

func Example_historyKey() {
	fmt.Println(historyKey(strings.Repeat("0", 64)))
	// Output: _demarkus/v1/history/0000000000000000000000000000000000000000000000000000000000000000.json
}
