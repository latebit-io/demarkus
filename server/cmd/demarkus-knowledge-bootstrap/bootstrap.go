package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
	"github.com/latebit-io/demarkus/server/internal/knowledge/knowledgeseed"
)

// policyMetadata is shared with the server's default seed so the two
// entry points cannot drift on the catalog axes a policy must carry.
var policyMetadata = knowledgeseed.PolicyMetadata()

func bootstrap(ctx context.Context, objects blob.Store, worldID string, policy []byte) error {
	seed := bucketstore.PolicySeed{Body: policy, Metadata: policyMetadata}
	if err := bucketstore.ValidatePolicySeed(seed); err != nil {
		return err
	}
	if err := bucketstore.Initialize(ctx, objects, worldID); err != nil {
		return fmt.Errorf("initialize bucket: %w", err)
	}
	store, err := bucketstore.Open(ctx, objects, bucketstore.Options{WorldID: worldID})
	if err != nil {
		return fmt.Errorf("open initialized bucket: %w", err)
	}
	if _, err := store.SeedPolicy(seed); err != nil {
		return err
	}
	// Unlike the server, the operator named an exact policy: an existing
	// document that differs is a mistake to report, not a world to adopt.
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		return fmt.Errorf("read seeded policy: %w", err)
	}
	if err := verifyPolicy(document, policy); err != nil {
		return err
	}
	if _, err := bucketstore.Open(ctx, objects, bucketstore.Options{WorldID: worldID, RequirePolicy: true}); err != nil {
		return fmt.Errorf("verify required policy: %w", err)
	}
	return nil
}

func verifyPolicy(document *protocolstore.Document, policy []byte) error {
	if document == nil {
		return errors.New("verify policy: store returned no document")
	}
	if document.Archived {
		return errors.New("verify policy: existing policy is archived")
	}
	if !bytes.Equal(document.Content, policy) {
		return errors.New("verify policy: existing policy differs; refusing to overwrite")
	}
	for key, expected := range policyMetadata {
		if document.Metadata[key] != expected {
			return fmt.Errorf("verify policy: existing metadata %q is %q, want %q; refusing to overwrite", key, document.Metadata[key], expected)
		}
	}
	return nil
}
