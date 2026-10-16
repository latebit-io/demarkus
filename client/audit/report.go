package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Markdown renders the shape /soul-doctor promises: heading with the count,
// a summary line, one section per check with its count and the count apply
// fixes, one bullet per finding, then Coverage.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s hygiene report  (%d docs)\n\n%d docs, %d findings", r.Scope, r.Documents, r.Documents, len(r.Findings))
	if r.Promotion {
		fmt.Fprintf(&b, ", %d already promoted", r.Promoted)
	}
	b.WriteByte('\n')
	hidden, fixable := 0, map[string]int{}
	byCheck := map[string][]Finding{}
	for _, f := range r.Findings {
		byCheck[f.Check] = append(byCheck[f.Check], f)
		if f.Fixable {
			fixable[f.Check]++
		}
	}
	for _, check := range CheckOrder {
		fs := byCheck[check]
		if len(fs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s (%d", check, len(fs))
		if n := fixable[check]; n > 0 {
			fmt.Fprintf(&b, ", %d fixable", n)
		}
		b.WriteString(")\n")
		for _, f := range fs[:min(len(fs), MaxShownPerCheck)] {
			fmt.Fprintf(&b, "- %s: %s", f.Path, f.Detail)
			if f.Fix != "" {
				b.WriteString("; fix: " + f.Fix)
			}
			b.WriteByte('\n')
		}
		if len(fs) > MaxShownPerCheck {
			fmt.Fprintf(&b, "- and %d more\n", len(fs)-MaxShownPerCheck)
			hidden += len(fs) - MaxShownPerCheck
		}
	}
	b.WriteString("\n### Coverage\n")
	if hidden > 0 {
		fmt.Fprintf(&b, "- %d findings not shown (%d per check); run on a subdirectory for the rest\n", hidden, MaxShownPerCheck)
	}
	if len(r.Coverage) == 0 {
		suffix := "; version history not checked (run with --deep)"
		if r.Deep {
			suffix = ", every version check run"
		}
		b.WriteString("- complete: every document in scope read" + suffix + "\n")
	}
	for _, c := range r.Coverage {
		b.WriteString("- " + c + "\n")
	}
	return b.String()
}

// JSON renders the report as data.
func (r *Report) JSON() (string, error) {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode report: %w", err)
	}
	return string(raw), nil
}
