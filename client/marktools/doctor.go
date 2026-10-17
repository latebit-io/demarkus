package marktools

import (
	"context"
	"time"

	"github.com/latebit-io/demarkus/client/audit"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// doctorTimeout bounds one audit from a tool call: an agent waits on the
// answer, so the CLI's ten minute envelope would outlive the host's patience.
const doctorTimeout = 2 * time.Minute

// DoctorArgs are mark_doctor's arguments: the scope URL (a store root or a
// directory under it) and the optional checks.
type DoctorArgs struct {
	URL       string
	Deep      bool
	Promotion bool
}

// Doctor answers mark_doctor: one read-only audit of the scope, rendered as
// the hygiene report. An audit that cannot be trusted is the failure text.
func (t *Tools) Doctor(ctx context.Context, args DoctorArgs) Result {
	target, bad := t.resolve(ctx, args.URL)
	if bad != nil {
		return *bad
	}
	report, bad := t.runAudit(ctx, target, audit.Options{Deep: args.Deep, Promotion: args.Promotion})
	if bad != nil {
		return *bad
	}
	return text(report.Markdown())
}

// runAudit runs one audit of the target's directory under the floor policy,
// bounded for a tool call.
func (t *Tools) runAudit(ctx context.Context, target Target, opts audit.Options) (*audit.Report, *Result) {
	floor, bad := t.floorPolicy(ctx, target)
	if bad != nil {
		return nil, bad
	}
	opts.Scope, opts.Timeout, opts.Floor = target.Path, doctorTimeout, floor
	store := &audit.ClientStore{Client: t.backend, Host: target.Host, Token: t.readToken(ctx, target.Host)}
	report, err := audit.Run(ctx, store, opts)
	if err != nil {
		r := failure("doctor failed: %v", err)
		return nil, &r
	}
	return report, nil
}

// floorPolicy reads the floor store's policy when the surface names one;
// a floor that cannot be read fails the audit, since every policy finding
// would then be judged without it.
func (t *Tools) floorPolicy(ctx context.Context, target Target) (*publishpolicy.Policy, *Result) {
	if t.hooks.Floor == nil {
		return nil, nil
	}
	floor, ok := t.hooks.Floor(ctx, target)
	if !ok {
		return nil, nil
	}
	policy, bad := t.worldPolicy(ctx, floor)
	if bad != nil {
		return nil, bad
	}
	return policy, nil
}
