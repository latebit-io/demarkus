// Package gate: documentation-style guard. The rules live in the shared
// client/docwrite/gate package, which the MCP write path runs too; this
// file only applies the hook's strictness to what they report.
package gate

import (
	"fmt"

	docgate "github.com/latebit-io/demarkus/client/docwrite/gate"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
)

// styleDecision runs the shared style rules on a publish (appends are
// fragments) at the configured strictness. Default warn; guide names the rules.
func styleDecision(pt config.ParsedTool, args map[string]any, guide string) (*Decision, error) {
	if pt.Verb != "publish" {
		return nil, nil
	}
	body, _ := args["body"].(string)
	description, _ := metadataOf(args)["description"].(string)
	problems := docgate.Problems(urlOf(args), body, description)
	if len(problems) == 0 {
		return nil, nil
	}
	reason := fmt.Sprintf(
		"demarkus publish to %s has style problems: %s DEMARKUS_STYLE_STRICTNESS adjusts this check (warn/ask/block).",
		urlOr(args, "the document"), docgate.Findings(problems, guide))
	s, err := config.StyleStrictness()
	if err != nil {
		return nil, err
	}
	return &Decision{Decision: string(s), Reason: reason}, nil
}
