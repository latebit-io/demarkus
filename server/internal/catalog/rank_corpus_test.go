package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol/store"
)

// TestRankCorpus reports the target's body-mode rank per benchmark question
// over a real corpus, for tuning weights without a full MCP run. It runs
// only with DEMARKUS_RANK_CORPUS (a file-store root) and
// DEMARKUS_RANK_QUESTIONS (a retrieval-bench fixture) set; DEMARKUS_RANK_SHOW
// lists question ids whose top rows to print.
func TestRankCorpus(t *testing.T) {
	root, questions := os.Getenv("DEMARKUS_RANK_CORPUS"), os.Getenv("DEMARKUS_RANK_QUESTIONS")
	if root == "" || questions == "" {
		t.Skip("set DEMARKUS_RANK_CORPUS and DEMARKUS_RANK_QUESTIONS")
	}
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	cat := New()
	if err := st.WalkCurrent(func(d store.CurrentDoc) error {
		cat.Put(d.Path, d.Metadata, d.Body, d.Modified)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(questions)
	if err != nil {
		t.Fatal(err)
	}
	// Mirrors retrievalbench.QuestionSet, which this module cannot import.
	var fixture struct {
		Scope     string `json:"scope"`
		Questions []struct {
			ID             string `json:"id"`
			Category       string `json:"category"`
			Query          string `json:"query"`
			Question       string `json:"question"`
			ExpectedPath   string `json:"expected_path"`
			ExpectedAnchor string `json:"expected_anchor"`
		} `json:"questions"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Questions) == 0 {
		t.Fatalf("%s: no questions", questions)
	}
	show := os.Getenv("DEMARKUS_RANK_SHOW")
	var sb strings.Builder
	over, hits, exact := 0, 0, 0
	for _, q := range fixture.Questions {
		rs, err := cat.Lookup(q.Query, Options{Match: MatchBody, Max: 10})
		if err != nil {
			t.Fatal(err)
		}
		rank, exactRank := 0, 0
		for i := range rs {
			if rs[i].Path != q.ExpectedPath {
				continue
			}
			if rank == 0 {
				rank = i + 1
			}
			if exactRank == 0 && (rs[i].Anchor == q.ExpectedAnchor || rs[i].Anchor == "") {
				exactRank = i + 1
			}
		}
		if rank > 0 && rank <= 5 {
			hits++
			over += rank - 1
		}
		if exactRank > 0 && exactRank <= 5 {
			exact++
		}
		fmt.Fprintf(&sb, "  %s rank=%d exact=%d\n", q.ID, rank, exactRank)
		if strings.Contains(show, q.ID) {
			for i := range rs[:min(5, len(rs))] {
				fmt.Fprintf(&sb, "      %d %.2f terms=%d %s\n", i+1, rs[i].Importance, len(rs[i].terms), rs[i].Location())
			}
		}
	}
	t.Logf("hits(top5)=%d exact=%d decoys-above=%d\n%s", hits, exact, over, sb.String())
}
