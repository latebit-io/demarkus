package merge

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiff3(t *testing.T) {
	tests := []struct {
		name         string
		base         string
		ours         string
		theirs       string
		wantBody     string
		wantConflict bool
	}{
		{
			name:         "all identical",
			base:         "a\nb\nc\n",
			ours:         "a\nb\nc\n",
			theirs:       "a\nb\nc\n",
			wantBody:     "a\nb\nc\n",
			wantConflict: false,
		},
		{
			name:         "only ours changed",
			base:         "a\nb\nc\n",
			ours:         "a\nB\nc\n",
			theirs:       "a\nb\nc\n",
			wantBody:     "a\nB\nc\n",
			wantConflict: false,
		},
		{
			name:         "only theirs changed",
			base:         "a\nb\nc\n",
			ours:         "a\nb\nc\n",
			theirs:       "a\nb\nC\n",
			wantBody:     "a\nb\nC\n",
			wantConflict: false,
		},
		{
			name:         "both sides changed identically",
			base:         "a\nb\nc\n",
			ours:         "a\nB\nc\n",
			theirs:       "a\nB\nc\n",
			wantBody:     "a\nB\nc\n",
			wantConflict: false,
		},
		{
			name:         "disjoint paragraph edits merge cleanly",
			base:         "para1\n\npara2\n\npara3\n",
			ours:         "PARA1\n\npara2\n\npara3\n",
			theirs:       "para1\n\npara2\n\nPARA3\n",
			wantBody:     "PARA1\n\npara2\n\nPARA3\n",
			wantConflict: false,
		},
		{
			name:         "same line changed differently produces conflict",
			base:         "a\nb\nc\n",
			ours:         "a\nB\nc\n",
			theirs:       "a\nXX\nc\n",
			wantBody:     "a\n<<<<<<< ours\nB\n=======\nXX\n>>>>>>> theirs\nc\n",
			wantConflict: true,
		},
		{
			name:         "ours inserts, theirs untouched",
			base:         "a\nc\n",
			ours:         "a\nb\nc\n",
			theirs:       "a\nc\n",
			wantBody:     "a\nb\nc\n",
			wantConflict: false,
		},
		{
			name:         "ours appends, theirs appends elsewhere",
			base:         "head\nmiddle\ntail\n",
			ours:         "head\nmiddle\ntail\nours-end\n",
			theirs:       "theirs-start\nhead\nmiddle\ntail\n",
			wantBody:     "theirs-start\nhead\nmiddle\ntail\nours-end\n",
			wantConflict: false,
		},
		{
			name:         "ours deletes, theirs untouched",
			base:         "a\nb\nc\n",
			ours:         "a\nc\n",
			theirs:       "a\nb\nc\n",
			wantBody:     "a\nc\n",
			wantConflict: false,
		},
		{
			name:         "empty base, both add same content",
			base:         "",
			ours:         "added\n",
			theirs:       "added\n",
			wantBody:     "added\n",
			wantConflict: false,
		},
		{
			name:         "empty base, both add different content conflicts",
			base:         "",
			ours:         "ours-only\n",
			theirs:       "theirs-only\n",
			wantBody:     "<<<<<<< ours\nours-only\n=======\ntheirs-only\n>>>>>>> theirs\n",
			wantConflict: true,
		},
		{
			name:         "all empty",
			base:         "",
			ours:         "",
			theirs:       "",
			wantBody:     "",
			wantConflict: false,
		},
		{
			name:         "ours empty, theirs unchanged means delete-vs-keep wins as delete",
			base:         "a\nb\n",
			ours:         "",
			theirs:       "a\nb\n",
			wantBody:     "",
			wantConflict: false,
		},
		{
			name:         "missing trailing newline still gets clean conflict markers",
			base:         "a\nb\nc",
			ours:         "a\nB\nc",
			theirs:       "a\nXX\nc",
			wantBody:     "a\n<<<<<<< ours\nB\n=======\nXX\n>>>>>>> theirs\nc",
			wantConflict: true,
		},
		{
			name:         "adjacent line edits do not over-conflict",
			base:         "a\nb\nc\nd\ne\n",
			ours:         "a\nB\nc\nd\ne\n",
			theirs:       "a\nb\nc\nD\ne\n",
			wantBody:     "a\nB\nc\nD\ne\n",
			wantConflict: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff3(tt.base, tt.ours, tt.theirs)
			if got.Body != tt.wantBody {
				t.Errorf("body mismatch\n  want: %q\n  got:  %q", tt.wantBody, got.Body)
			}
			if got.Conflict != tt.wantConflict {
				t.Errorf("conflict mismatch: want %v, got %v", tt.wantConflict, got.Conflict)
			}
		})
	}
}

func TestDiff3_OversizedInputsFallBackToSingleHunk(t *testing.T) {
	// Build inputs whose product exceeds maxLCSCells. lcsMatches returns nil,
	// editHunks emits a single whole-range hunk, and diff3 then handles the
	// chunk normally — identical inputs collapse to base, divergent inputs
	// produce one big conflict block. Either way, no allocation explosion.
	side := func(line string, n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "%s %d\n", line, i)
		}
		return b.String()
	}
	// 1500 * 1500 = 2.25M > maxLCSCells (2M).
	const n = 1500

	t.Run("identical large inputs merge cleanly", func(t *testing.T) {
		body := side("line", n)
		got := Diff3(body, body, body)
		if got.Conflict {
			t.Error("identical inputs should never conflict")
		}
		if got.Body != body {
			t.Errorf("identical merge corrupted body (len got=%d want=%d)", len(got.Body), len(body))
		}
	})

	t.Run("divergent large inputs yield one conflict block", func(t *testing.T) {
		base := side("base", n)
		ours := side("ours", n)
		theirs := side("theirs", n)
		got := Diff3(base, ours, theirs)
		if !got.Conflict {
			t.Error("divergent inputs should conflict")
		}
		// Exactly one conflict block since the fallback emits one big hunk.
		if c := strings.Count(got.Body, "<<<<<<< ours"); c != 1 {
			t.Errorf("want exactly 1 conflict block, got %d", c)
		}
	})
}

func TestDiff3_NoConflictsHaveMarkers(t *testing.T) {
	// Property: if Conflict is false, the merged body must not contain
	// any conflict markers — agents downstream rely on this to decide
	// whether to invoke an LLM.
	got := Diff3("a\nb\nc\n", "a\nB\nc\n", "a\nb\nC\n")
	if got.Conflict {
		t.Fatal("expected clean merge")
	}
	for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if strings.Contains(got.Body, marker) {
			t.Errorf("clean merge body contains marker %q: %q", marker, got.Body)
		}
	}
}
