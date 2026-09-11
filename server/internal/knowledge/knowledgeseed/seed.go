// Package knowledgeseed holds the documents a knowledge world is seeded
// with the first time it is created.
package knowledgeseed

import (
	"bytes"
	_ "embed"
	"maps"
)

// policyBody is the default write policy. Directives parse anywhere in the
// body, so prose must never begin a line with one.
//
//go:embed policy.md
var policyBody []byte

// policyMetadata is the catalog metadata a seeded policy carries. The
// category axis is set so a later category requirement cannot lock out
// edits to the policy itself.
var policyMetadata = map[string]string{
	"title":      "Knowledge System Policy",
	"tags":       "category:governance",
	"importance": "1",
	"type":       "Reference",
}

// PolicyBody returns a copy of the default write policy document.
func PolicyBody() []byte {
	return bytes.Clone(policyBody)
}

// PolicyMetadata returns a copy of the default policy's catalog metadata.
func PolicyMetadata() map[string]string {
	return maps.Clone(policyMetadata)
}
