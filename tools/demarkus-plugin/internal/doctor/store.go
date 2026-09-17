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
func (s *ClientStore) List(_ context.Context, dir string, includeArchived bool, cursor string) (protocol.Response, error) {
	res, err := s.Client.ListWithOptions(s.Host, dir, s.Token, fetch.ListOptions{IncludeArchived: includeArchived, Cursor: cursor})
	return res.Response, err
}

// Fetch fetches one document or version.
func (s *ClientStore) Fetch(ctx context.Context, docPath string) (protocol.Response, error) {
	res, err := s.Client.FetchContext(ctx, s.Host, docPath, s.Token)
	return res.Response, err
}

// Versions fetches a document's version history.
func (s *ClientStore) Versions(_ context.Context, docPath string) (protocol.Response, error) {
	res, err := s.Client.Versions(s.Host, docPath, s.Token)
	return res.Response, err
}
