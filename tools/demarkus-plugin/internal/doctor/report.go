package doctor

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Markdown renders the report in the shape the /soul-doctor prompt promises:
// heading with the document count, one summary line, one section per check
// with its count, one bullet per finding, then Coverage.
func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s hygiene report  (%d docs)\n\n%d docs, %d findings\n", r.Scope, r.Documents, r.Documents, len(r.Findings))
	byCheck := map[string][]Finding{}
	for _, f := range r.Findings {
		byCheck[f.Check] = append(byCheck[f.Check], f)
	}
	for _, check := range CheckOrder {
		fs := byCheck[check]
		if len(fs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n### %s (%d)\n", check, len(fs))
		for _, f := range fs {
			fmt.Fprintf(&b, "- %s: %s", f.Path, f.Detail)
			if f.Fix != "" {
				b.WriteString("; fix: " + f.Fix)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n### Coverage\n")
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
