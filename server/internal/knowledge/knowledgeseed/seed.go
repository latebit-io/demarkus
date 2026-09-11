// Package knowledgeseed holds the documents a knowledge world is seeded
// with the first time it is created.
package knowledgeseed

import (
	"bytes"
	_ "embed"
	"fmt"
	"maps"
	"os"

	"github.com/latebit-io/demarkus/protocol"
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

// PolicySeedFromFile reads an operator-supplied policy body and returns it
// as a validated seed carrying the same catalog metadata as the default, so
// the two sources cannot drift on the axes a policy must have.
func PolicySeedFromFile(name string) (bucketstore.PolicySeed, error) {
	body, err := readPolicyFile(name)
	if err != nil {
		return bucketstore.PolicySeed{}, err
	}
	seed := bucketstore.PolicySeed{Body: body, Metadata: maps.Clone(policyMetadata)}
	if err := bucketstore.ValidatePolicySeed(seed); err != nil {
		return bucketstore.PolicySeed{}, fmt.Errorf("policy file %q: %w", name, err)
	}
	return seed, nil
}

func readPolicyFile(name string) ([]byte, error) {
	// Stat before open: opening a FIFO blocks, and a device reads unbounded.
	info, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("stat policy file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("policy file %q must be a regular file", name)
	}
	if info.Size() > protocol.MaxBodyLength {
		return nil, fmt.Errorf("policy file %q exceeds %d bytes", name, protocol.MaxBodyLength)
	}
	body, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	return body, nil
}
