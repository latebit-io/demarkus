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

func TestCandidate(t *testing.T) {
	t.Run("first publish succeeds", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{{Status: "ok", Version: 6}},
		}
		out, err := Candidate(c, "/p", "body", 5, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeOK {
			t.Errorf("want OutcomeOK, got %s", out.Status)
		}
		if out.Publish.Version != 6 {
			t.Errorf("want version 6, got %d", out.Publish.Version)
		}
		if out.Body != "" {
			t.Errorf("OutcomeOK should have empty Body, got %q", out.Body)
		}
		if len(c.publishedBody) != 1 {
			t.Errorf("want 1 publish call, got %d", len(c.publishedBody))
		}
	})

	t.Run("disjoint conflict produces clean candidate", func(t *testing.T) {
		// Agent edited base v5 ("a\nb\nc\n") into "a\nB\nc\n".
		// Latest is v6 ("a\nb\nC\n"). diff3 produces "a\nB\nC\n".
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\nb\nc\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nb\nC\n", Version: 6},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
			},
		}
		out, err := Candidate(c, "/p", "a\nB\nc\n", 5, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeCandidate {
			t.Errorf("want OutcomeCandidate, got %s", out.Status)
		}
		if out.HasMarkers {
			t.Error("want HasMarkers=false")
		}
		if out.Body != "a\nB\nC\n" {
			t.Errorf("candidate body: want %q, got %q", "a\nB\nC\n", out.Body)
		}
		if out.BaseVersion != 5 || out.TheirVersion != 6 || out.PublishAtVersion != 6 {
			t.Errorf("versions: want base=5 their=6 publishAt=6, got base=%d their=%d publishAt=%d",
				out.BaseVersion, out.TheirVersion, out.PublishAtVersion)
		}
		// Tool never auto-publishes — only the initial attempt happened.
		if len(c.publishedBody) != 1 {
			t.Errorf("want exactly 1 publish call (no auto-publish of merge), got %d", len(c.publishedBody))
		}
	})

	t.Run("overlapping conflict produces candidate with markers", func(t *testing.T) {
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
		out, err := Candidate(c, "/p", "a\nB\nc\n", 5, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeCandidate {
			t.Errorf("want OutcomeCandidate, got %s", out.Status)
		}
		if !out.HasMarkers {
			t.Error("want HasMarkers=true")
		}
		want := "a\n<<<<<<< ours\nB\n=======\nXX\n>>>>>>> theirs\nc\n"
		if out.Body != want {
			t.Errorf("body mismatch\n  want: %q\n  got:  %q", want, out.Body)
		}
	})

	t.Run("create-only conflict merges against empty base", func(t *testing.T) {
		// Agent tries to create with body "x\ny\n", but doc already exists
		// at v3 with body "x\nz\n". With empty base, ours and theirs both
		// add lines starting from nothing — diff3 detects overlap.
		c := &stubClient{
			currentDocs: []Doc{
				{Status: "ok", Body: "x\nz\n", Version: 3},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 3},
			},
		}
		out, err := Candidate(c, "/p", "x\ny\n", 0, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.Status != OutcomeCandidate {
			t.Errorf("want OutcomeCandidate, got %s", out.Status)
		}
		if out.BaseVersion != 0 {
			t.Errorf("want BaseVersion=0, got %d", out.BaseVersion)
		}
		if out.PublishAtVersion != 3 {
			t.Errorf("want PublishAtVersion=3, got %d", out.PublishAtVersion)
		}
	})

	t.Run("invalid expected_version", func(t *testing.T) {
		_, err := Candidate(&stubClient{}, "/p", "b", -1, nil)
		if !errors.Is(err, ErrInvalidExpectedVersion) {
			t.Errorf("want ErrInvalidExpectedVersion, got %v", err)
		}
	})

	t.Run("metadata is forwarded on publish", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{{Status: "ok", Version: 6}},
		}
		meta := map[string]string{"agent": "test"}
		_, err := Candidate(c, "/p", "body", 5, meta)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := c.publishedMeta[0]["agent"]; got != "test" {
			t.Errorf("agent metadata lost: %q", got)
		}
	})

	t.Run("base fetch failure propagates", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
			},
			fetchErr: errors.New("network down"),
		}
		_, err := Candidate(c, "/p", "body", 5, nil)
		if err == nil {
			t.Fatal("expected error from base fetch")
		}
	})

	t.Run("agent loop simulation: re-call with candidate body succeeds", func(t *testing.T) {
		// Round 1: publish v5 → conflict, get candidate at v6.
		// Round 2: agent reviews candidate, calls Candidate again
		// with the candidate body and PublishAtVersion=6 → succeeds.
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "a\nb\nc\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "a\nb\nC\n", Version: 6},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
				{Status: "ok", Version: 7},
			},
		}
		out1, err := Candidate(c, "/p", "a\nB\nc\n", 5, nil)
		if err != nil {
			t.Fatalf("round 1: %v", err)
		}
		if out1.Status != OutcomeCandidate {
			t.Fatalf("round 1: want OutcomeCandidate, got %s", out1.Status)
		}

		// Agent's semantic verification is a no-op for this test.
		out2, err := Candidate(c, "/p", out1.Body, out1.PublishAtVersion, nil)
		if err != nil {
			t.Fatalf("round 2: %v", err)
		}
		if out2.Status != OutcomeOK {
			t.Errorf("round 2: want OutcomeOK, got %s", out2.Status)
		}
		if out2.Publish.Version != 7 {
			t.Errorf("round 2: want version 7, got %d", out2.Publish.Version)
		}
		if got := c.publishedExp[1]; got != 6 {
			t.Errorf("round 2: expected_version sent = %d, want 6", got)
		}
		if got := c.publishedBody[1]; got != "a\nB\nC\n" {
			t.Errorf("round 2: body sent = %q, want %q", got, "a\nB\nC\n")
		}
	})
}
