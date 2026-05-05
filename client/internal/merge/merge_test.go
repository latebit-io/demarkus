package merge

import (
	"errors"
	"testing"
)

// stubClient is a programmable Client for tests. Each Publish call advances
// through publishResults; each FetchCurrent through currentDocs.
type stubClient struct {
	versionedDocs  map[int]Doc     // FetchVersion lookups
	currentDocs    []Doc           // FetchCurrent returns these in order
	publishResults []PublishResult // Publish returns these in order
	publishErrs    []error         // optional errors aligned with publishResults
	fetchErr       error           // returned by FetchVersion if non-nil
	currentErr     error           // returned by FetchCurrent if non-nil
	publishedBody  []string        // bodies seen by Publish, in order
	publishedMeta  []map[string]string
	publishedExp   []int
	currentIdx     int
	publishIdx     int
}

func (s *stubClient) FetchVersion(_ string, version int) (Doc, error) {
	if s.fetchErr != nil {
		return Doc{}, s.fetchErr
	}
	d, ok := s.versionedDocs[version]
	if !ok {
		return Doc{Status: "not-found"}, nil
	}
	return d, nil
}

func (s *stubClient) FetchCurrent(_ string) (Doc, error) {
	if s.currentErr != nil {
		return Doc{}, s.currentErr
	}
	if s.currentIdx >= len(s.currentDocs) {
		return Doc{}, errors.New("stub: no more current docs")
	}
	d := s.currentDocs[s.currentIdx]
	s.currentIdx++
	return d, nil
}

func (s *stubClient) Publish(_, body string, expectedVersion int, meta map[string]string) (PublishResult, error) {
	s.publishedBody = append(s.publishedBody, body)
	s.publishedMeta = append(s.publishedMeta, meta)
	s.publishedExp = append(s.publishedExp, expectedVersion)
	if s.publishIdx < len(s.publishErrs) && s.publishErrs[s.publishIdx] != nil {
		err := s.publishErrs[s.publishIdx]
		s.publishIdx++
		return PublishResult{}, err
	}
	if s.publishIdx >= len(s.publishResults) {
		return PublishResult{}, errors.New("stub: no more publish results")
	}
	r := s.publishResults[s.publishIdx]
	s.publishIdx++
	return r, nil
}

func TestMergeAndPublish(t *testing.T) {
	t.Run("first publish succeeds without merge", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{{Status: "ok", Version: 6}},
		}
		out, err := MergeAndPublish(c, "/p", "body", 5, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeOK {
			t.Errorf("want OutcomeOK, got %s", out.Status)
		}
		if out.Merged {
			t.Error("Merged should be false on first-try success")
		}
		if out.Publish.Version != 6 {
			t.Errorf("want version 6, got %d", out.Publish.Version)
		}
	})

	t.Run("disjoint changes merge cleanly on retry", func(t *testing.T) {
		// Agent edited base v5 ("a\nb\nc\n") into "a\nB\nc\n".
		// Meanwhile latest is v6 ("a\nb\nC\n").
		// diff3 should merge to "a\nB\nC\n".
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\nb\nc\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nb\nC\n", Version: 6},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6}, // first try
				{Status: "ok", Version: 7},             // merged publish
			},
		}
		out, err := MergeAndPublish(c, "/p", "a\nB\nc\n", 5, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeOK {
			t.Errorf("want OutcomeOK, got %s", out.Status)
		}
		if !out.Merged {
			t.Error("Merged should be true")
		}
		if out.Publish.Version != 7 {
			t.Errorf("want version 7, got %d", out.Publish.Version)
		}
		if got := c.publishedBody[1]; got != "a\nB\nC\n" {
			t.Errorf("merged body: want %q, got %q", "a\nB\nC\n", got)
		}
		if got := c.publishedExp[1]; got != 6 {
			t.Errorf("publish expected_version: want 6, got %d", got)
		}
		if out.Retries != 1 {
			t.Errorf("want Retries=1, got %d", out.Retries)
		}
	})

	t.Run("overlapping changes return OutcomeConflict with marked body", func(t *testing.T) {
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\nb\nc\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nXX\nc\n", Version: 6},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
			},
		}
		out, err := MergeAndPublish(c, "/p", "a\nB\nc\n", 5, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeConflict {
			t.Fatalf("want OutcomeConflict, got %s", out.Status)
		}
		if !out.Merged {
			t.Error("Merged should be true")
		}
		if out.BaseVersion != 5 || out.TheirVersion != 6 {
			t.Errorf("versions: want base=5 their=6, got base=%d their=%d", out.BaseVersion, out.TheirVersion)
		}
		want := "a\n<<<<<<< ours\nB\n=======\nXX\n>>>>>>> theirs\nc\n"
		if out.ConflictBody != want {
			t.Errorf("conflict body mismatch\n  want: %q\n  got:  %q", want, out.ConflictBody)
		}
		// Only one publish (the initial) — merged version was never sent.
		if len(c.publishedBody) != 1 {
			t.Errorf("want 1 publish call, got %d", len(c.publishedBody))
		}
	})

	t.Run("third writer slips in, merge loop resolves", func(t *testing.T) {
		// Agent edits v5 → "a\nB\nc\n".
		// Latest is v6 ("a\nb\nC\n"). First merge yields "a\nB\nC\n", republish.
		// Third writer pushed v7 ("a\nb\nC\nadded\n") in the meantime.
		// Republish conflicts. Re-merge: base=v6, ours="a\nB\nC\n", theirs=v7.
		// diff3 yields "a\nB\nC\nadded\n". Republish at v7 succeeds.
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\nb\nc\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nb\nC\n", Version: 6},
				{Status: "ok", Body: "a\nb\nC\nadded\n", Version: 7},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6}, // initial
				{Status: "conflict", ServerVersion: 7}, // first merged retry
				{Status: "ok", Version: 8},             // second merged retry
			},
		}
		out, err := MergeAndPublish(c, "/p", "a\nB\nc\n", 5, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeOK {
			t.Fatalf("want OutcomeOK, got %s", out.Status)
		}
		if out.Retries != 2 {
			t.Errorf("want Retries=2, got %d", out.Retries)
		}
		if got := c.publishedBody[2]; got != "a\nB\nC\nadded\n" {
			t.Errorf("final body: want %q, got %q", "a\nB\nC\nadded\n", got)
		}
		if got := c.publishedExp[2]; got != 7 {
			t.Errorf("final publish expected_version: want 7, got %d", got)
		}
	})

	t.Run("contention exhausts retries", func(t *testing.T) {
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "x\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "x\n", Version: 6},
				{Status: "ok", Body: "x\n", Version: 7},
				{Status: "ok", Body: "x\n", Version: 8},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6}, // initial
				{Status: "conflict", ServerVersion: 7},
				{Status: "conflict", ServerVersion: 8},
				{Status: "conflict", ServerVersion: 9},
			},
		}
		out, err := MergeAndPublish(c, "/p", "y\n", 5, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeContention {
			t.Errorf("want OutcomeContention, got %s", out.Status)
		}
		if out.Retries != 3 {
			t.Errorf("want Retries=3, got %d", out.Retries)
		}
		if out.TheirVersion != 8 {
			t.Errorf("want TheirVersion=8 (last fetched), got %d", out.TheirVersion)
		}
	})

	t.Run("create-only conflict returns OutcomeConflict without merge", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 3},
			},
		}
		out, err := MergeAndPublish(c, "/p", "body", 0, 3, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeConflict {
			t.Errorf("want OutcomeConflict, got %s", out.Status)
		}
		if out.Merged {
			t.Error("expected_version=0 path should never merge")
		}
		if out.TheirVersion != 3 {
			t.Errorf("want TheirVersion=3, got %d", out.TheirVersion)
		}
	})

	t.Run("invalid expected_version", func(t *testing.T) {
		_, err := MergeAndPublish(&stubClient{}, "/p", "b", -1, 3, nil)
		if !errors.Is(err, ErrInvalidExpectedVersion) {
			t.Errorf("want ErrInvalidExpectedVersion, got %v", err)
		}
	})

	t.Run("metadata is forwarded on every publish", func(t *testing.T) {
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\n"},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nB\n", Version: 6},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
				{Status: "ok", Version: 7},
			},
		}
		meta := map[string]string{"agent": "test"}
		_, err := MergeAndPublish(c, "/p", "a\n", 5, 3, meta)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i, m := range c.publishedMeta {
			if m["agent"] != "test" {
				t.Errorf("publish #%d: agent metadata lost: %v", i, m)
			}
		}
	})
}
