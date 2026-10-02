// Package bucketstore stores one knowledge world in a bucket: a log of
// create-only slots over a verified checkpoint (ADR 0036).
package bucketstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// schemaVersion versions the checkpoint objects (root, shards, manifests,
	// history); logSchema versions the world marker, checkpoints and slots.
	schemaVersion    = 1
	logSchema        = 2
	shardCount       = 256
	maximumDocuments = 100_000
	historyBlockSize = 256
	// maxSlotEntries bounds one slot's batch, and readers find the slot holding
	// a sequence within this many names below it: raising it needs a new
	// logSchema.
	maxSlotEntries = 32

	objectPrefix     = "_demarkus/v1/"
	markerKey        = objectPrefix + "head.json"
	checkpointPrefix = objectPrefix + "checkpoints/"
	logPrefix        = objectPrefix + "log/"
)

type objectRef struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
}

type historyEntry struct {
	Version  int       `json:"version"`
	Blob     objectRef `json:"blob"`
	BodyHash string    `json:"body_hash"`
	Modified string    `json:"modified"`
}

type historyObject struct {
	Schema   int            `json:"schema"`
	PathHash string         `json:"path_hash"`
	First    int            `json:"first"`
	Last     int            `json:"last"`
	Entries  []historyEntry `json:"entries"`
}

type historyRef struct {
	PathHash string `json:"path_hash"`
	First    int    `json:"first"`
	Last     int    `json:"last"`
	objectRef
}

type manifestObject struct {
	Schema   int          `json:"schema"`
	PathHash string       `json:"path_hash"`
	Current  int          `json:"current"`
	Archived bool         `json:"archived"`
	History  []historyRef `json:"history"`
}

type catalogRecord struct {
	Path       string            `json:"path"`
	Title      string            `json:"title"`
	Tags       []string          `json:"tags"`
	Importance string            `json:"importance"`
	Modified   string            `json:"modified"`
	Metadata   map[string]string `json:"metadata"`
}

type shardEntry struct {
	Path     string        `json:"path"`
	PathHash string        `json:"path_hash"`
	Manifest objectRef     `json:"manifest"`
	Current  int           `json:"current"`
	Archived bool          `json:"archived"`
	BodyHash string        `json:"body_hash"`
	Modified string        `json:"modified"`
	Catalog  catalogRecord `json:"catalog"`
}

type shardObject struct {
	Schema  int          `json:"schema"`
	Shard   string       `json:"shard"`
	Entries []shardEntry `json:"entries"`
}

type shardRef struct {
	Shard string `json:"shard"`
	objectRef
}

type rootObject struct {
	Schema        int        `json:"schema"`
	WorldID       string     `json:"world_id"`
	DocumentCount int        `json:"document_count"`
	Shards        []shardRef `json:"shards"`
}

// markerObject is head.json under schema 2: written once when the world is
// created, never on the commit path. A replica on schema 1 refuses it.
type markerObject struct {
	Schema  int    `json:"schema"`
	WorldID string `json:"world_id"`
}

// checkpointObject names the root that holds the world through Sequence.
// Tip is the hash the next slot names as its predecessor, empty when no slot
// precedes it.
type checkpointObject struct {
	Schema   int       `json:"schema"`
	WorldID  string    `json:"world_id"`
	Sequence int64     `json:"sequence"`
	Root     objectRef `json:"root"`
	Tip      string    `json:"tip"`
}

// slotObject is one commit: a batch of changes with contiguous sequences from
// First, chained to its predecessor by Prev.
type slotObject struct {
	Schema  int         `json:"schema"`
	WorldID string      `json:"world_id"`
	First   int64       `json:"first"`
	Store   string      `json:"store"`
	Prev    string      `json:"prev"`
	Entries []slotEntry `json:"entries"`
}

// slotEntry is one committed change and the document state after it. A write
// carries its catalog record and version; an archive transition carries
// neither and keeps the document's.
type slotEntry struct {
	OperationID string         `json:"operation_id"`
	Op          string         `json:"op"`
	Agent       string         `json:"agent,omitempty"`
	Path        string         `json:"path"`
	Current     int            `json:"current"`
	First       int            `json:"first"`
	Archived    bool           `json:"archived"`
	BodyHash    string         `json:"body_hash"`
	Modified    string         `json:"modified"`
	Catalog     *catalogRecord `json:"catalog,omitempty"`
	Version     *historyEntry  `json:"version,omitempty"`
}

func (slot *slotObject) last() int64 { return slot.First + int64(len(slot.Entries)) - 1 }

type modelObject struct {
	Key  string
	Data []byte
}

func immutableJSON(key func(string) string, value any) (modelObject, objectRef, error) {
	data, err := marshalImmutable(value)
	if err != nil {
		return modelObject{}, objectRef{}, err
	}
	hash := hashHex(data)
	objectKey := key(hash)
	return modelObject{Key: objectKey, Data: data}, objectRef{Key: objectKey, Hash: hash}, nil
}

func marshalImmutable(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical JSON: %w", err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("marshal canonical JSON: invalid output")
	}
	return data, nil
}

func decodeImmutable(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode canonical JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode canonical JSON: trailing value")
		}
		return fmt.Errorf("decode canonical JSON trailer: %w", err)
	}
	canonical, err := marshalImmutable(target)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("JSON is not canonical compact encoding")
	}
	return nil
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func pathHash(path string) string {
	return hashHex([]byte(path))
}

func blobKey(hash string) string {
	return objectPrefix + "blobs/" + hash
}

func historyKey(hash string) string {
	return objectPrefix + "history/" + hash + ".json"
}

func manifestKey(pathHash, hash string) string {
	return objectPrefix + "docs/" + pathHash + "/manifests/" + hash + ".json"
}

func shardKey(shard, hash string) string {
	return objectPrefix + "index/" + shard + "/" + hash + ".json"
}

func rootKey(hash string) string {
	return objectPrefix + "roots/" + hash + ".json"
}

func checkpointKey(sequence int64) string {
	return fmt.Sprintf("%s%016x.json", checkpointPrefix, sequence)
}

func slotKey(first int64) string {
	return fmt.Sprintf("%s%016x.json", logPrefix, first)
}

// sequenceOfKey parses the sequence a checkpoint or slot key names.
func sequenceOfKey(key, prefix string) (int64, bool) {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok || len(name) != 16+len(".json") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	sequence, err := strconv.ParseInt(name[:16], 16, 64)
	if err != nil || sequence < 1 || fmt.Sprintf("%016x", sequence) != name[:16] {
		return 0, false
	}
	return sequence, true
}
