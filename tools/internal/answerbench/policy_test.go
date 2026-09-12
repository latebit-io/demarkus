package answerbench

import (
	"strings"
	"testing"
)

func policyReport(t *testing.T, name string) Report {
	t.Helper()
	r := comparisonReport()
	r.Spec.Suite, r.Spec.Port, r.Spec.Steps = "graph-answer-v1", 16319, 8
	r.Spec.ReaderPolicy = name
	prompt, err := policyPrompt(name)
	if err != nil {
		t.Fatal(err)
	}
	r.Spec.PromptHash = digest([]byte(prompt))
	raw, err := readerConfig(&Config{Proxy: "<runner>", MCP: "<mcp>", Port: 16319, Steps: 8, ReaderPolicy: name}, "<session>")
	if err != nil {
		t.Fatal(err)
	}
	r.Spec.ConfigHash = digest(raw)
	r.Binaries = map[string]string{"server": "same-server", "mcp": "same-mcp"}
	return r
}

func TestPolicyComparisonPermitsOnlyDeclaredReaderChange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*Report)
		wantError bool
	}{
		{"policy-only", func(*Report) {}, false},
		{"different-software", func(r *Report) { r.Binaries["mcp"] = "changed" }, true},
		{"different-model", func(r *Report) { r.Spec.Model = "different/model" }, true},
		{"different-corpus", func(r *Report) { r.Spec.Hashes = map[string]string{"corpus": "changed"} }, true},
		{"arbitrary-prompt", func(r *Report) { r.Spec.PromptHash = "changed" }, true},
		{"arbitrary-config", func(r *Report) { r.Spec.ConfigHash = "changed" }, true},
		{"unknown-policy", func(r *Report) { r.Spec.ReaderPolicy = "unknown" }, true},
		{"incomplete-usage", func(r *Report) { r.Attempts[0].Trace.UsageComplete = false }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, after := policyReport(t, ""), policyReport(t, "section-first")
			tc.mutate(&after)
			if _, err := ComparePolicies(&before, &after); (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%t", err, tc.wantError)
			}
			if _, err := Compare(&before, &after); err == nil {
				t.Fatal("strict comparison accepted a policy change")
			}
		})
	}
}

func TestPolicyPreservesGradingContractAndLegacyPrompt(t *testing.T) {
	if digest([]byte(readerPrompt)) != "sha256-58cff26fda42d64522eb8350512e2541214525322682c0e4e8a69134056d1b3e" {
		t.Fatal("original baseline prompt changed")
	}
	_, contract, _ := strings.Cut(readerPrompt, "\n")
	after, err := policyPrompt("section-first")
	if err != nil || !strings.HasSuffix(after, "\n"+contract) {
		t.Fatalf("grading/citation instructions changed: %v", err)
	}
	before, compatible := policyReport(t, ""), policyReport(t, "budget-body")
	if _, err := Compare(&before, &compatible); err != nil {
		t.Fatalf("legacy implicit policy rejected: %v", err)
	}
}
