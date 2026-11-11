package marktools

import (
	"context"

	"github.com/latebit-io/demarkus/client/memory/landing"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// StyleGate is how a surface holds a publish to the style rules: block
// refuses the write, anything else lets it land with the findings in a note.
// Ask has nobody to answer inside a server, so it lands as warn (ADR 0040).
type StyleGate struct {
	Strictness publishpolicy.Strictness
	Guide      string // what a finding cites; "" cites gate.Baseline
}

// stylePolicy asks the surface how it holds a publish to target to the
// style rules; nil when it has no say, which warns and cites the baseline.
func (t *Tools) stylePolicy(ctx context.Context, target Target) func() landing.Style {
	if t.hooks.Style == nil {
		return nil
	}
	return func() landing.Style {
		policy := t.hooks.Style(ctx, target)
		return landing.Style{Strictness: policy.Strictness, Guide: policy.Guide}
	}
}
