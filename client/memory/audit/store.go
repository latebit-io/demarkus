package audit

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// Reader is the part of a Mark client an audit reads through; *fetch.Client
// and the MCP surfaces' backends both satisfy it.
type Reader interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
	Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error)
}

// ClientStore adapts a Reader to the Store interface for one host and token.
type ClientStore struct {
	Client Reader
	Host   string // host:port, or a world name behind the broker
	Token  string
}

// Authority is the host the audit resolves the store's own links against.
func (s *ClientStore) Authority() string { return s.Host }

// List fetches one LIST page.
func (s *ClientStore) List(ctx context.Context, dir string, includeArchived bool, cursor string) (protocol.Response, error) {
	res, err := s.Client.List(ctx, fetch.ListRequest{Host: s.Host, Path: dir, Token: s.Token, IncludeArchived: includeArchived, Cursor: cursor})
	return res.Response, err
}

// Fetch fetches one document or version.
func (s *ClientStore) Fetch(ctx context.Context, docPath string) (protocol.Response, error) {
	res, err := s.Client.Fetch(ctx, fetch.FetchRequest{Host: s.Host, Path: docPath, Token: s.Token})
	return res.Response, err
}

// Versions fetches a document's version history.
func (s *ClientStore) Versions(ctx context.Context, docPath string) (protocol.Response, error) {
	res, err := s.Client.Versions(ctx, fetch.VersionsRequest{Host: s.Host, Path: docPath, Token: s.Token})
	return res.Response, err
}
