package broker

import (
	"context"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/oauthsrv"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/storage"
)

// RevokeSessionsOptions names the identity whose logins end: an IdP subject
// or an email.
type RevokeSessionsOptions struct {
	ConfigPath string
	Identity   string
	Log        *slog.Logger
}

// revokeSessionsTimeout bounds one run; it reads every login record once.
const revokeSessionsTimeout = 5 * time.Minute

// RunRevokeSessions is the operator's revocation: every refresh login of the
// identity ends, in the state bucket the serving pods read, so none of them
// mints for it again. A bearer already minted lasts out its idTokenTTL.
func RunRevokeSessions(ctx context.Context, opts RevokeSessionsOptions) (revoked int, err error) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, revokeSessionsTimeout)
	defer cancel()
	cfg, err := core.LoadConfig(opts.ConfigPath)
	if err != nil {
		return 0, err
	}
	gcsClient, cleanup, err := storage.NewGCSClient(opts.Log)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	state, err := storage.OpenStateBucket(gcsClient, &cfg.Server, opts.Log)
	if err != nil {
		return 0, err
	}
	return oauthsrv.NewRefreshStore(cfg, state, opts.Log).RevokeIdentity(ctx, opts.Identity)
}
