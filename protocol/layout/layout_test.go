package layout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

func TestParseOverlaysDirectivesOnDefault(t *testing.T) {
	body := "# Template\n\n```text\njournal: log\nplans: work/\nroadmap: next.md\nstaging: log, scratch\npromote_types: Decision, ADR\npromote_floor: 0.6\n```\n\njournal: ignored-second-occurrence\n"
	got, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Journal != "log/" || got.Plans != "work/" || got.Roadmap != "next.md" || got.PromoteFloor != 0.6 {
		t.Errorf("parsed = %+v", got)
	}
	if fmt.Sprint(got.Staging) != "[log scratch]" || fmt.Sprint(got.PromoteTypes) != "[Decision ADR]" {
		t.Errorf("lists = %v %v", got.Staging, got.PromoteTypes)
	}
}

func TestParseNamesRejectedKeysAndKeepsTheirDefaults(t *testing.T) {
	got, err := Parse("journal: ../etc\nplans: Two Words\nroadmap: /abs.md\nstaging: ok, ../bad\npromote_types: has space\npromote_floor: 7\n")
	if err == nil || err.Error() != "template /.well-known/demarkus/template.md: invalid journal, plans, roadmap, staging, promote_types, promote_floor" {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint(Default()) {
		t.Errorf("invalid values changed the layout: %+v", got)
	}
	if got, err := Parse(""); err != nil || fmt.Sprint(got) != fmt.Sprint(Default()) {
		t.Error("empty body is the default")
	}
}

func TestReadMapsStatusToLayout(t *testing.T) {
	answer := func(status, body string, err error) func(context.Context, string) (protocol.Response, error) {
		return func(_ context.Context, p string) (protocol.Response, error) {
			if p != DocumentPath {
				t.Errorf("read %s, want the template", p)
			}
			return protocol.Response{Status: status, Body: body}, err
		}
	}
	if l, err := Read(t.Context(), answer(protocol.StatusOK, "journal: log\n", nil)); err != nil || l.Journal != "log/" {
		t.Errorf("ok = %+v %v", l, err)
	}
	if l, err := Read(t.Context(), answer(protocol.StatusNotFound, "", nil)); err != nil || l.Journal != "journal/" {
		t.Errorf("not-found = %+v %v", l, err)
	}
	if _, err := Read(t.Context(), answer(protocol.StatusUnauthorized, "", nil)); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("unauthorized = %v", err)
	}
	if _, err := Read(t.Context(), answer("", "", errors.New("dial"))); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Errorf("transport = %v", err)
	}
	if _, err := Read(t.Context(), answer(protocol.StatusOK, "journal: Bad/\n", nil)); err == nil {
		t.Error("an invalid template is the reader's error")
	}
}

func TestRootProject(t *testing.T) {
	l, err := Parse("root_project: demarkus\n")
	if err != nil || l.Root("demarkus") != "/" || l.Root("demarkus-library") != "/demarkus-library/" {
		t.Errorf("root project = %+v %v", l, err)
	}
	def := Default()
	if def.Root("demarkus") != "/demarkus/" {
		t.Errorf("default root = %s", def.Root("demarkus"))
	}
	for _, v := range []string{"/", "/demarkus/", "", "Two Words"} {
		got, err := Parse("root_project: " + v + "\n")
		if err == nil || !strings.Contains(err.Error(), "invalid root_project") || got.RootProject != "" {
			t.Errorf("root_project %q = %+v %v", v, got, err)
		}
	}
}

func TestSection(t *testing.T) {
	if Section("journal/") != "Journal" || Section("log") != "Log" || Section("") != "" {
		t.Error("section names capitalize the directory")
	}
}

func TestIsJournalEntry(t *testing.T) {
	for name, want := range map[string]bool{
		"2026-10-13.md": true, "2026-02-30.md": false, "2026-10-13.txt": false,
		"notes.md": false, "2026-10-13-extra.md": false, "2026-1-3.md": false,
	} {
		if IsJournalEntry(name) != want {
			t.Errorf("IsJournalEntry(%q) = %v, want %v", name, !want, want)
		}
	}
}

// A dated entry is one directly in a journal directory, at any depth.
func TestJournalEntry(t *testing.T) {
	l := Default()
	for p, want := range map[string]bool{
		"/journal/2026-10-13.md": true, "/proj/journal/2026-10-13.md": true,
		"/plans/2026-10-13.md": false, "/journal/notes.md": false, "/journal/sub/2026-10-13.md": false,
	} {
		if l.JournalEntry(p) != want {
			t.Errorf("JournalEntry(%q) = %v, want %v", p, !want, want)
		}
	}
	if !l.JournalDir("/proj/journal/") || l.JournalDir("/plans/") {
		t.Error("JournalDir")
	}
}

func TestIsADRDir(t *testing.T) {
	for dir, want := range map[string]bool{"/adr/": true, "/cwd/adr": true, "/cwd/adr/old/": false, "/adrs/": false, "/": false} {
		if IsADRDir(dir) != want {
			t.Errorf("IsADRDir(%q) = %v", dir, !want)
		}
	}
}

func TestRepeatsByDesign(t *testing.T) {
	l := Default()
	for _, p := range []string{"/index.md", "/x/index.md", "/log.md", "/.well-known/demarkus/policy.md", "/journal/2026-10-21.md", "/proj/journal/2026-10-21.md"} {
		if !l.RepeatsByDesign(p) {
			t.Errorf("%s not exempt", p)
		}
	}
	for _, p := range []string{"/notes/journal/x.md", "/journal/notes.md", "/patterns.md", "/adr/0001-x.md"} {
		if l.RepeatsByDesign(p) {
			t.Errorf("%s exempt", p)
		}
	}
}
