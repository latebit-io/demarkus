package marktools

import (
	"context"

	"github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/client/mcpfmt"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
)

// StyleGate is how a surface holds a publish to the style rules: block
// refuses the write, anything else lets it land with the findings in a note.
// Ask has nobody to answer inside a server, so it lands as warn (ADR 0040).
type StyleGate struct {
	Strictness publishpolicy.Strictness
	Guide      string // what a finding cites; "" cites gate.Baseline
}

// styleGate runs the rules on a publish body. refused is the tool's answer
// when the gate blocks; otherwise note is what to append to a landed write.
func (t *Tools) styleGate(ctx context.Context, target Target, body, description string) (note string, refused *Result) {
	problems := gate.Problems(target.Path, body, description)
	if len(problems) == 0 {
		return "", nil
	}
	policy := StyleGate{Strictness: publishpolicy.Warn}
	if t.hooks.Style != nil {
		policy = t.hooks.Style(ctx, target)
	}
	if policy.Guide == "" {
		policy.Guide = gate.Baseline
	}
	findings := gate.Findings(problems, policy.Guide)
	if policy.Strictness == publishpolicy.Block {
		bad := failure("publish refused, style problems: %s Fix the body and republish.", findings)
		return "", &bad
	}
	return mcpfmt.Note("style problems in this publish: " + findings), nil
}
