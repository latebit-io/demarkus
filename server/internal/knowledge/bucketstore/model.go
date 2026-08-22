// Package bucketstore stores one knowledge world behind an immutable root graph.
package bucketstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const (
	schemaVersion    = 1
	shardCount       = 256
	maximumDocuments = 100_000
	historyBlockSize = 256
	maximumReceipts  = 16

	objectPrefix  = "_demarkus/v1/"
	headObjectKey = objectPrefix + "head.json"
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

type operationReceipt struct {
	OperationID string `json:"operation_id"`
	Sequence    int64  `json:"sequence"`
	Result      string `json:"result"`
}

type headObject struct {
	Schema   int                `json:"schema"`
	WorldID  string             `json:"world_id"`
	Sequence int64              `json:"sequence"`
	Root     objectRef          `json:"root"`
	Receipts []operationReceipt `json:"receipts"`
}

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
