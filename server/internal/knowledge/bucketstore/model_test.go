package bucketstore

import (
	"bytes"
	"fmt"
	"maps"
	"strconv"
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
				if got := validWorldID(test.value); got != test.valid {
					t.Errorf("validWorldID(%q) = %v, want %v", test.value, got, test.valid)
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
		path := strings.Repeat("b", 64)
		tests := []struct {
			name string
			got  string
			want string
		}{
			{name: "head", got: headObjectKey, want: "_demarkus/v1/head.json"},
			{name: "blob", got: blobKey(hash), want: "_demarkus/v1/blobs/" + hash},
			{name: "history", got: historyKey(hash), want: "_demarkus/v1/history/" + hash + ".json"},
			{name: "manifest", got: manifestKey(path, hash), want: "_demarkus/v1/docs/" + path + "/manifests/" + hash + ".json"},
			{name: "shard", got: shardKey("af", hash), want: "_demarkus/v1/index/af/" + hash + ".json"},
			{name: "root", got: rootKey(hash), want: "_demarkus/v1/roots/" + hash + ".json"},
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
				if got := validTimestamp(test.value); got != test.valid {
					t.Errorf("validTimestamp(%q) = %v, want %v", test.value, got, test.valid)
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
	head := headObject{
		Schema:   schemaVersion,
		WorldID:  testWorldID,
		Sequence: 1,
		Root:     objectRef{Key: rootKey(rootHash), Hash: rootHash},
		Receipts: make([]operationReceipt, 0),
	}
	canonical, err := marshalImmutable(head)
	if err != nil {
		t.Fatalf("marshal head: %v", err)
	}
	want := `{"schema":1,"world_id":"52b471f7-8d38-4c89-b44a-6f4f8b1a4f48","sequence":1,"root":{"key":"_demarkus/v1/roots/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"receipts":[]}`
	reordered := `{"world_id":"52b471f7-8d38-4c89-b44a-6f4f8b1a4f48","schema":1,"sequence":1,"root":{"key":"_demarkus/v1/roots/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json","hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"receipts":[]}`
	if string(canonical) != want {
		t.Fatalf("canonical head:\n%s\nwant:\n%s", canonical, want)
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
				var decoded headObject
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
		data := []byte(strings.Replace(want, `"receipts":[]`, `"receipts":null`, 1))
		var decoded headObject
		if err := decodeImmutable(data, &decoded); err != nil {
			t.Fatalf("decode canonical null: %v", err)
		}
		if err := validateHeadObject(&decoded); err == nil {
			t.Fatal("validateHeadObject() accepted null receipts")
		}
	})
}

func TestGoldenHistoryObject(t *testing.T) {
	pathSum := strings.Repeat("1", 64)
	blobSum := strings.Repeat("2", 64)
	bodySum := strings.Repeat("3", 64)
	history := historyObject{
		Schema:   schemaVersion,
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
	validRef := objectRef{Key: rootKey(hash), Hash: hash}
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
		{name: "head receipts", validate: func() error {
			return validateHeadObject(&headObject{Schema: schemaVersion, WorldID: testWorldID, Sequence: 1, Root: validRef})
		}},
		{name: "root shards", validate: func() error {
			return validateRootObject(&rootObject{Schema: schemaVersion, WorldID: testWorldID}, testWorldID)
		}},
		{name: "shard entries", validate: func() error {
			return validateShardObject(&shardObject{Schema: schemaVersion, Shard: "00"}, "00")
		}},
		{name: "history entries", validate: func() error {
			return validateHistoryObject(&historyObject{Schema: schemaVersion, PathHash: hash, First: 1, Last: 1})
		}},
		{name: "manifest history", validate: func() error {
			return validateManifestObject(&manifestObject{Schema: schemaVersion, PathHash: hash, Current: 1})
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

func TestReceiptWindowValidation(t *testing.T) {
	rootHash := strings.Repeat("a", 64)
	makeHead := func(sequence int64) headObject {
		receiptCount := int(min(sequence-1, int64(maximumReceipts)))
		receipts := make([]operationReceipt, receiptCount)
		for index := range receipts {
			receiptSequence := sequence - int64(receiptCount) + int64(index) + 1
			receipts[index] = operationReceipt{
				OperationID: fmt.Sprintf("00000000-0000-4000-8000-%012x", receiptSequence),
				Sequence:    receiptSequence,
				Result:      "committed",
			}
		}
		return headObject{
			Schema:   schemaVersion,
			WorldID:  testWorldID,
			Sequence: sequence,
			Root:     objectRef{Key: rootKey(rootHash), Hash: rootHash},
			Receipts: receipts,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*headObject)
		wantErr bool
	}{
		{name: "genesis", mutate: func(head *headObject) { *head = makeHead(1) }},
		{name: "partial window", mutate: func(head *headObject) { *head = makeHead(20) }},
		{name: "missing receipt", mutate: func(head *headObject) {
			*head = makeHead(3)
			head.Receipts = head.Receipts[1:]
		}, wantErr: true},
		{name: "sequence gap", mutate: func(head *headObject) {
			*head = makeHead(4)
			head.Receipts[1].Sequence++
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var head headObject
			test.mutate(&head)
			err := validateHeadObject(&head)
			if (err != nil) != test.wantErr {
				t.Errorf("validateHeadObject() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestHistoryBlockValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	ref := func(first, last int) historyRef {
		return historyRef{
			PathHash:  hash,
			First:     first,
			Last:      last,
			objectRef: objectRef{Key: historyKey(hash), Hash: hash},
		}
	}
	tests := []struct {
		name    string
		value   manifestObject
		wantErr bool
	}{
		{
			name: "absolute contiguous blocks",
			value: manifestObject{
				Schema: schemaVersion, PathHash: hash, Current: 300,
				History: []historyRef{ref(100, 256), ref(257, 300)},
			},
		},
		{
			name: "range crosses block",
			value: manifestObject{
				Schema: schemaVersion, PathHash: hash, Current: 257,
				History: []historyRef{ref(256, 257)},
			},
			wantErr: true,
		},
		{
			name: "gap",
			value: manifestObject{
				Schema: schemaVersion, PathHash: hash, Current: 300,
				History: []historyRef{ref(100, 250), ref(257, 300)},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateManifestObject(&test.value)
			if (err != nil) != test.wantErr {
				t.Errorf("validateManifestObject() error = %v, wantErr %v", err, test.wantErr)
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

func TestShardEntryRejectsUnaddressablePaths(t *testing.T) {
	paths := []string{
		"/line\nbreak.md",
		"/" + strings.Repeat("a", 4090) + ".md",
	}
	for _, path := range paths {
		t.Run(fmt.Sprintf("%d-bytes", len(path)), func(t *testing.T) {
			entry := testEntry(path, false, "")
			shard, err := strconv.ParseUint(entry.PathHash[:2], 16, 8)
			if err != nil {
				t.Fatalf("parse shard: %v", err)
			}
			if err := validateShardEntry(&entry, int(shard)); err == nil {
				t.Errorf("validateShardEntry() accepted %q", path)
			}
		})
	}
}

func TestShardEntryAllowsPathAgnosticDocuments(t *testing.T) {
	entry := testEntry("/x", false, "")
	shard, err := strconv.ParseUint(entry.PathHash[:2], 16, 8)
	if err != nil {
		t.Fatalf("parse shard: %v", err)
	}
	if err := validateShardEntry(&entry, int(shard)); err != nil {
		t.Errorf("validateShardEntry(/x): %v", err)
	}
}

func Example_historyKey() {
	fmt.Println(historyKey(strings.Repeat("0", 64)))
	// Output: _demarkus/v1/history/0000000000000000000000000000000000000000000000000000000000000000.json
}
