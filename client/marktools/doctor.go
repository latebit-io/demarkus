package marktools

import (
	"context"
	"time"

	"github.com/latebit-io/demarkus/client/audit"
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
	store := &audit.ClientStore{Client: t.backend, Host: target.Host, Token: t.readToken(ctx, target.Host)}
	report, err := audit.Run(ctx, store, audit.Options{Scope: target.Path, Deep: args.Deep, Promotion: args.Promotion, Timeout: doctorTimeout})
	if err != nil {
		return failure("doctor failed: %v", err)
	}
	return text(report.Markdown())
}
