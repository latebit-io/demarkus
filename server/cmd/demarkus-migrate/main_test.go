package main

import (
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
)

func TestDocDigestIncludesArchiveState(t *testing.T) {
	active := store.StoredDocument{Versions: []store.StoredVersion{{Version: 1, Stored: []byte("stored")}}}
	archived := active
	archived.Archived = true
	if docDigest(active) == docDigest(archived) {
		t.Fatal("archive state did not change document digest")
	}
}
