package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/broker/core"
)

// agentTokenInterval bounds how long a deleted agent Secret or reset world
// tokens Secret stays broken.
const agentTokenInterval = time.Minute

// AgentTokens issues the agents' publish tokens (cfg.AgentTokens). A broker
// record marks an agent Secret as managed; one without a record is left alone.
// Every replica runs it; each step converges on the first committed record.
type AgentTokens struct {
	cfg      *core.Config
	store    core.SecretStore
	log      *slog.Logger
	interval time.Duration
	// unmanaged dedupes the "left alone" log line per world.
	unmanaged map[string]bool
}

// NewAgentTokens builds the reconciler; a nil log means slog.Default.
func NewAgentTokens(cfg *core.Config, store core.SecretStore, log *slog.Logger) *AgentTokens {
	if log == nil {
		log = slog.Default()
	}
	return &AgentTokens{cfg: cfg, store: store, log: log, interval: agentTokenInterval, unmanaged: make(map[string]bool)}
}

// Run reconciles at once, so a fresh install's agent starts without waiting
// an interval, then every interval until ctx ends. Failures are logged.
func (a *AgentTokens) Run(ctx context.Context) {
	a.runOnce(ctx)
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runOnce(ctx)
		}
	}
}

func (a *AgentTokens) runOnce(ctx context.Context) {
	if err := a.Reconcile(ctx); err != nil && ctx.Err() == nil {
		a.log.ErrorContext(ctx, "broker: agent token reconcile failed", "err", err)
	}
}

// Reconcile runs one pass over every entry; one entry failing does not stop the rest.
func (a *AgentTokens) Reconcile(ctx context.Context) error {
	var errs []error
	for i := range a.cfg.AgentTokens {
		spec := &a.cfg.AgentTokens[i]
		if err := a.reconcileOne(ctx, spec); err != nil {
			errs = append(errs, fmt.Errorf("agent token %s: %w", spec.World, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileOne writes record, then world hash, then agent Secret, so the world
// usually knows the hash before the agent's pod can start on the raw value.
func (a *AgentTokens) reconcileOne(ctx context.Context, spec *core.AgentTokenConfig) error {
	world := core.LookupWorld(a.cfg.Registry(), spec.World)
	if world == nil {
		return fmt.Errorf("world %q not found", spec.World)
	}
	recordRef := core.AgentTokenRecordRef(a.cfg, spec.World)
	agentRef := core.AgentTokenRef(world, spec)

	record, managed, err := a.record(ctx, spec, recordRef, agentRef)
	if err != nil || !managed {
		return err
	}
	if err := SyncWorldHash(ctx, a.store, world, &record); err != nil {
		return err
	}
	written := false
	err = a.store.Mutate(ctx, agentRef, func(current []byte) ([]byte, error) {
		raw := []byte(record.RawToken)
		written = !bytes.Equal(current, raw)
		return raw, nil
	})
	if err != nil {
		return err
	}
	if written {
		a.log.InfoContext(ctx, "broker: wrote agent token Secret", "world", spec.World, "secret", agentRef.String())
	}
	return nil
}

// record returns the broker's record for spec, minting one unless the agent
// Secret already exists without a record (unmanaged: managed is false).
func (a *AgentTokens) record(ctx context.Context, spec *core.AgentTokenConfig, recordRef, agentRef core.SecretRef) (TokenRecord, bool, error) {
	existing, err := core.ReadSecret(ctx, a.store, recordRef)
	if err != nil {
		return TokenRecord{}, false, err
	}
	if len(existing) > 0 {
		record, err := decodeTokenRecord(recordRef, existing)
		return record, err == nil, err
	}
	current, err := core.ReadSecret(ctx, a.store, agentRef)
	if err != nil {
		return TokenRecord{}, false, err
	}
	if len(current) > 0 {
		if !a.unmanaged[spec.World] {
			a.unmanaged[spec.World] = true
			a.log.InfoContext(ctx, "broker: agent token Secret exists without a broker record; leaving it alone",
				"world", spec.World, "secret", agentRef.String())
		}
		return TokenRecord{}, false, nil
	}
	record, err := EnsureTokenRecord(ctx, a.store, recordRef, TokenMint{Label: "agent-" + spec.World, Paths: spec.Paths})
	return record, err == nil, err
}
