package answerbench

import (
	"fmt"
	"strings"

	"github.com/latebit-io/demarkus/tools/internal/retrievalbench"
)

const sectionFirstRetrieval = `Answer using only the frozen knowledge MCP tools. Choose your own search terms and navigation. Target at most two tool calls and about 1500 total tool-result tokens for ordinary recall, without sacrificing source support.
Use lookup to locate evidence, then fetch the specific #anchor. Omit budget by default; start with limit 3. For named documents use catalog lookup; use match=body when the question concerns text inside a section or catalog lookup misses. Use descriptive topic terms rather than a bare numeric ID across the entire corpus. Scope url to the relevant subtree when the question or observed links establish it; otherwise start at / and narrow from results. Do not invent document paths.
Read the best matching section rather than expanding unrelated candidates. A short document without a returned anchor can be fetched whole. For an outline, choose the relevant section from that outline. For a historical question, fetch the requested revision and use its headings. Only use budgeted body lookup when a focused query is likely to directly return the needed passage; keep that optional budget at or below 1500. If evidence is missing, refine the query or scope within the remaining steps rather than guessing. Source facts may differ across revisions or regions. Do not guess missing facts.`

func policyName(name string) string {
	if name == "" {
		return "budget-body"
	}
	return name
}

func policyPrompt(name string) (string, error) {
	switch policyName(name) {
	case "budget-body":
		return readerPrompt, nil
	case "section-first":
		// Keep the answer/citation contract byte-identical across reader policies.
		_, contract, found := strings.Cut(readerPrompt, "\n")
		if !found {
			return "", fmt.Errorf("reader answer contract missing")
		}
		return sectionFirstRetrieval + "\n" + contract, nil
	default:
		return "", fmt.Errorf("unknown reader policy %q", name)
	}
}

func verifyPolicyConfig(spec *RunSpec) error {
	prompt, err := policyPrompt(spec.ReaderPolicy)
	if err != nil {
		return err
	}
	if spec.PromptHash != digest([]byte(prompt)) {
		return fmt.Errorf("reader prompt does not match its named policy")
	}
	cfg := Config{Proxy: "<runner>", MCP: "<mcp>", Origin: spec.Origin, Port: spec.Port, Steps: spec.Steps, ReaderPolicy: spec.ReaderPolicy}
	switch spec.Suite {
	case "soul-answer-v1":
		cfg.Corpus = "<snapshot>"
	case "graph-answer-v1":
	default:
		return fmt.Errorf("unknown policy experiment suite %q", spec.Suite)
	}
	raw, err := readerConfig(&cfg, "<session>")
	if err != nil {
		return err
	}
	if spec.ConfigHash != digest(raw) {
		return fmt.Errorf("reader configuration changed beyond the named policy")
	}
	return nil
}

// ComparePolicies permits only the known reader-prompt change, keeping software,
// model, corpus, scoring, and all other experiment settings fixed.
func ComparePolicies(before, after *Report) (Comparison, error) {
	for _, report := range []*Report{before, after} {
		if err := verifyPolicyConfig(&report.Spec); err != nil {
			return Comparison{}, err
		}
	}
	for _, binary := range []string{"server", "mcp"} {
		if before.Binaries[binary] == "" || before.Binaries[binary] != after.Binaries[binary] {
			return Comparison{}, fmt.Errorf("%s binary differs; policy-only comparison requires identical retrieval software", binary)
		}
	}
	b, a := *before, *after
	b.Spec.ReaderPolicy, a.Spec.ReaderPolicy = "", ""
	b.Spec.PromptHash, a.Spec.PromptHash = "", ""
	b.Spec.ConfigHash, a.Spec.ConfigHash = "", ""
	c, err := Compare(&b, &a)
	if err != nil {
		return c, err
	}
	c.BeforePolicy, c.AfterPolicy = policyName(before.Spec.ReaderPolicy), policyName(after.Spec.ReaderPolicy)
	c.Note = "Reader-policy ablation with identical retrieval binaries, model, corpus and scoring. Point estimates only; not a software optimization or significance claim."
	for _, item := range []struct {
		report *Report
		total  **int
	}{{before, &c.BeforeResultTokens}, {after, &c.AfterResultTokens}} {
		total, err := reportResultTokens(item.report)
		if err != nil {
			return c, err
		}
		*item.total = &total
	}
	return c, nil
}

func reportResultTokens(report *Report) (int, error) {
	if report.MetricsOnly {
		if report.ResultTokens == nil || *report.ResultTokens < 0 {
			return 0, fmt.Errorf("metrics-only report lacks tool-result token count")
		}
		return *report.ResultTokens, nil
	}
	counter, err := retrievalbench.NewO200kCounter()
	if err != nil {
		return 0, err
	}
	total := 0
	for i := range report.Attempts {
		for _, call := range report.Attempts[i].Trace.Calls {
			n, err := counter.Count(call.Output)
			if err != nil {
				return 0, err
			}
			total += n
		}
	}
	return total, nil
}
