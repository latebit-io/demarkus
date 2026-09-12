// Package knowledgeseed holds the documents a knowledge world is seeded
// with the first time it is created.
package knowledgeseed

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"maps"
	"os"
	"syscall"

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
	// O_NONBLOCK: a FIFO at this path would otherwise park the world open
	// until a writer appeared, and no context can interrupt that.
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open policy file: %w", err)
	}
	// Read-only, so a close error cannot lose data.
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat policy file: %w", err)
	}
	// On the descriptor, not the path: nothing can be swapped in behind it.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("policy file %q must be a regular file", name)
	}
	body, err := io.ReadAll(io.LimitReader(file, protocol.MaxBodyLength+1))
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	if len(body) > protocol.MaxBodyLength {
		return nil, fmt.Errorf("policy file %q exceeds %d bytes", name, protocol.MaxBodyLength)
	}
	return body, nil
}
