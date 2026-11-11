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
			{Check: audit.CheckStyle, Path: "/a.md", Detail: "no summary", FixKind: audit.FixCurate, Fixable: true},
			{Check: audit.CheckPromotion, Path: "/b.md", Detail: "durable", FixKind: audit.FixPromote},
		},
	}
	out := report.Markdown()
	for _, want := range []string{"/soul-curate /a.md", "/promote /b.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mark_curate") {
		t.Errorf("the batch fix names an MCP tool, not the slash command:\n%s", out)
	}

	// Of another store the report names no command: they write to the bound one.
	report.Commands = audit.Commands{}
	if neutral := report.Markdown(); strings.Contains(neutral, "/soul-") || strings.Contains(neutral, "/promote") {
		t.Errorf("a report without commands still cites one:\n%s", neutral)
	}
	if note := otherStoreNote("team", "soul"); !strings.Contains(note, "`team`") || !strings.Contains(note, "bound to `soul`") {
		t.Errorf("note = %q, want both stores named", note)
	}
	if note := otherStoreNote("team", ""); !strings.Contains(note, "no usable binding") {
		t.Errorf("note = %q, want the missing binding said", note)
	}
}
