package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	protocolstore "github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"github.com/latebit-io/demarkus/server/internal/knowledge/bucketstore"
)

var policyMetadata = map[string]string{
	"title":      "Knowledge System Policy",
	"tags":       "category:governance",
	"importance": "1",
	"type":       "Reference",
}

func bootstrap(ctx context.Context, objects blob.Store, worldID string, policy []byte) error {
	if err := validatePolicy(policy); err != nil {
		return err
	}
	if err := bucketstore.Initialize(ctx, objects, worldID); err != nil {
		return fmt.Errorf("initialize bucket: %w", err)
	}
	store, err := bucketstore.Open(ctx, objects, bucketstore.Options{WorldID: worldID, RequirePolicy: false})
	if err != nil {
		return fmt.Errorf("open initialized bucket: %w", err)
	}
	if err := seedPolicy(store, policy); err != nil {
		return err
	}
	if _, err := bucketstore.Open(ctx, objects, bucketstore.Options{WorldID: worldID, RequirePolicy: true}); err != nil {
		return fmt.Errorf("verify required policy: %w", err)
	}
	return nil
}

func validatePolicy(policy []byte) error {
	if err := protocolstore.ValidateDocumentContent(publishpolicy.DocumentPath, policy); err != nil {
		return fmt.Errorf("invalid policy document: %w", err)
	}
	if err := protocolstore.ValidateWrite(policy, policyMetadata); err != nil {
		return fmt.Errorf("invalid policy write: %w", err)
	}
	parsed := publishpolicy.Parse(string(policy))
	if err := parsed.Validate(); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

func seedPolicy(store *bucketstore.Store, policy []byte) error {
	document, err := store.Get(publishpolicy.DocumentPath, 0)
	switch {
	case err == nil:
		return verifyPolicy(document, policy)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("check existing policy: %w", err)
	}

	document, err = store.WriteVersion(publishpolicy.DocumentPath, 0, policy, policyMetadata)
	if err == nil {
		return verifyPolicy(document, policy)
	}
	if !errors.Is(err, protocolstore.ErrConflict) {
		return fmt.Errorf("create policy: %w", err)
	}
	document, err = store.Get(publishpolicy.DocumentPath, 0)
	if err != nil {
		return fmt.Errorf("read concurrently created policy: %w", err)
	}
	return verifyPolicy(document, policy)
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
