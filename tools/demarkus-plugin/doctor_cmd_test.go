package main

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/memory/audit"
)

func TestDoctorReportCitesSlashCommands(t *testing.T) {
	report := &audit.Report{
		Scope:    "/",
		Commands: soulCommands(),
		Findings: []audit.Finding{
			{Check: audit.CheckStyle, Path: "/a.md", Detail: "no summary", FixKind: audit.FixCurate},
			{Check: audit.CheckPromotion, Path: "/b.md", Detail: "durable", FixKind: audit.FixPromote},
		},
	}
	out := report.Markdown()
	for _, want := range []string{"/soul-curate /a.md", "/promote /b.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}
