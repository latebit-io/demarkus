package knowledgeserver

import (
	"context"
	"fmt"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/snirouter"
	"github.com/latebit-io/demarkus/server/internal/worldruntime"
)

// ErrUnknownAuthority is Exchange's answer for an authority no live world
// routes.
var ErrUnknownAuthority = snirouter.ErrUnknownAuthority

// Routes reports whether a live world serves authority right now.
func (s *Server) Routes(authority string) bool {
	_, err := s.worlds.Router().Lookup(authority)
	return err == nil
}

// Exchange answers req through the world that routes authority, in process,
// under a QUIC stream's controls plus any protocol.Grant on ctx. A cancelled
// ctx ends reads at once; a commit under way seals first and is reported.
func (s *Server) Exchange(ctx context.Context, authority string, req protocol.Request) (protocol.Response, error) {
	if err := ctx.Err(); err != nil {
		return protocol.Response{}, err
	}
	endpoint, err := s.worlds.Router().Lookup(authority)
	if err != nil {
		return protocol.Response{}, err
	}
	exchanger, ok := endpoint.(worldruntime.Exchanger)
	if !ok {
		return protocol.Response{}, fmt.Errorf("authority %s: endpoint %T serves streams only", authority, endpoint)
	}
	return exchanger.Exchange(ctx, localAddr{}, req)
}

// localAddr is the remote of an in-process stream; the rate limiter keys on
// it, so local callers share one bucket, as one broker pod does over QUIC.
type localAddr struct{}

func (localAddr) Network() string { return "local" }
func (localAddr) String() string  { return "local" }
