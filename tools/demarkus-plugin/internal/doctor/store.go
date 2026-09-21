package doctor

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// ClientStore adapts a Mark client to the Store interface for one host and token.
type ClientStore struct {
	Client *fetch.Client
	Host   string // host:port
	Token  string
}

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
