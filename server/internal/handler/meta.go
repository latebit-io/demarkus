package handler

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/hiddentext"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

// extractPublisherMeta returns non-control metadata keys from a request.
// Returns nil if no publisher keys are present. Count and size are writeMeta's
// to judge, after the server's own keys are set.
func extractPublisherMeta(reqMeta map[string]string) (map[string]string, error) {
	var meta map[string]string
	for k, v := range reqMeta {
		if protocol.IsControlMetadataKey(k) {
			continue
		}
		if protocol.IsReservedMetadataKey(k) {
			return nil, fmt.Errorf("metadata key %q is reserved", k)
		}
		if protocol.IsServerStampedKey(k) {
			continue
		}
		if !protocol.IsValidMetaKey(k) {
			return nil, fmt.Errorf("metadata key %q contains invalid characters", k)
		}
		if !protocol.IsValidMetaValue(v) {
			return nil, fmt.Errorf("metadata value for key %q contains newlines", k)
		}
		if hits := hiddentext.Find(v); len(hits) > 0 {
			return nil, fmt.Errorf("metadata value for key %q carries hidden characters: %s", k, hiddentext.Summary(hits))
		}
		if meta == nil {
			meta = make(map[string]string)
		}
		meta[k] = v
	}
	return meta, nil
}

// writeMeta is the metadata a write stores: the request's publisher keys
// under the grant's user, judged by the store's rules. A PUBLISH is typed
// here; an APPEND in the commit, after the base's metadata is merged.
func writeMeta(ctx context.Context, req *protocol.Request) (map[string]string, error) {
	meta, err := extractPublisherMeta(req.Metadata)
	if err != nil {
		return nil, err
	}
	meta = stampUser(ctx, meta)
	if req.Verb == protocol.VerbPublish {
		meta = storefmt.ApplyOKFTypeDefault(req.Path, meta)
	}
	return meta, storefmt.ValidateMeta(meta)
}

// stampUser makes the write's user key the grant's: the person its issuer
// verified, or none. A request's own value never lands, and a capability
// token names nobody; the handler copies the stamp without reading it.
func stampUser(ctx context.Context, meta map[string]string) map[string]string {
	delete(meta, protocol.MetaUser)
	grant, ok := protocol.GrantFrom(ctx)
	if !ok || grant.User == "" {
		return meta
	}
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta[protocol.MetaUser] = grant.User
	return meta
}

// copyPublisherMeta copies stored metadata into dst, filtering out any
// reserved or control keys. This prevents tampered version files from
// leaking server-owned keys into responses.
func copyPublisherMeta(dst, src map[string]string) {
	for k, v := range src {
		if protocol.IsReservedMetadataKey(k) || protocol.IsControlMetadataKey(k) {
			continue
		}
		if !protocol.IsValidMetaKey(k) || !protocol.IsValidMetaValue(v) {
			continue
		}
		dst[k] = v
	}
}
