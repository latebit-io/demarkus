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
			fixable[""]++
		}
	}
	if n := fixable[""]; n > 0 {
		fmt.Fprintf(&b, "%d fixable: mark_curate %s proposes the writes\n", n, r.Scope)
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

// Markdown renders the proposal: the hash, each write with its changes (a
// created document in full, since nothing else shows it), then what the
// plan leaves to judgment.
func (p *Plan) Markdown(scope string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s fix proposal\n\n", scope)
	if len(p.Writes) == 0 {
		b.WriteString("nothing to apply\n")
	} else {
		fmt.Fprintf(&b, "proposal: %s\nwrites: %d", p.Hash, len(p.Writes))
		if p.More > 0 {
			fmt.Fprintf(&b, " of %d; apply, then rerun for the next batch", len(p.Writes)+p.More)
		}
		b.WriteString("\n\n### Writes\n")
	}
	for i := range p.Writes {
		w := &p.Writes[i]
		at := fmt.Sprintf("v%d", w.Version)
		if w.Version == 0 {
			at = "create"
		}
		if len(w.Requires) > 0 {
			at += ", after " + strings.Join(w.Requires, ", ")
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", w.Path, at, strings.Join(w.Fixes, "; "))
		if w.Version == 0 {
			fmt.Fprintf(&b, "\n~~~~markdown\n%s~~~~\n\n", w.Body)
		}
	}
	if len(p.Unfixed) > 0 {
		fmt.Fprintf(&b, "\n### Left to judgment (%d)\n", len(p.Unfixed))
		for _, f := range p.Unfixed {
			fmt.Fprintf(&b, "- %s: %s: %s\n", f.Check, f.Path, f.Detail)
		}
	}
	if len(p.Writes) > 0 {
		b.WriteString("\napply=true with the proposal hash writes the batch\n")
	}
	return b.String()
}

// ApplyMarkdown renders an apply: a count, one line per write, and what a
// rerun takes.
func ApplyMarkdown(scope string, p *Plan, outcomes []Outcome) string {
	applied := 0
	var b strings.Builder
	for i := range outcomes {
		o := &outcomes[i]
		switch {
		case o.Result != Applied:
			fmt.Fprintf(&b, "- %s: %s: %s\n", o.Path, o.Result, o.Detail)
			continue
		case o.Detail != "":
			fmt.Fprintf(&b, "- %s: applied, %s\n", o.Path, o.Detail)
		default:
			fmt.Fprintf(&b, "- %s: applied v%d\n", o.Path, o.Version)
		}
		applied++
	}
	head := fmt.Sprintf("status: applied %d of %d\n", applied, len(outcomes))
	if p.More > 0 || applied < len(outcomes) {
		b.WriteString("\nrerun mark_curate " + scope + " for the rest and review it before applying\n")
	}
	return head + b.String()
}
