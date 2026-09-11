// Package knowledgeseed holds the documents a knowledge world is seeded
// with the first time it is created.
package knowledgeseed

import (
	"bytes"
	_ "embed"
	"maps"

	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
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

// DefaultPolicySeed returns the default write policy as a fresh seed, so a
// caller can neither mutate the embedded body nor share its metadata map.
func DefaultPolicySeed() bucketstore.PolicySeed {
	return bucketstore.PolicySeed{
		Body:     bytes.Clone(policyBody),
		Metadata: maps.Clone(policyMetadata),
	}
}
