package merge

import (
	"context"
	"errors"
	"strings"
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
	contexts       []context.Context // one per call, in order
}

func (s *stubClient) FetchVersion(ctx context.Context, _ string, version int) (Doc, error) {
	s.contexts = append(s.contexts, ctx)
	if s.fetchErr != nil {
		return Doc{}, s.fetchErr
	}
	d, ok := s.versionedDocs[version]
	if !ok {
		return Doc{Status: "not-found"}, nil
	}
	return d, nil
}

func (s *stubClient) FetchCurrent(ctx context.Context, _ string) (Doc, error) {
	s.contexts = append(s.contexts, ctx)
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

func (s *stubClient) Publish(ctx context.Context, w Write) (PublishResult, error) {
	s.contexts = append(s.contexts, ctx)
	s.publishedBody = append(s.publishedBody, w.Body)
	s.publishedMeta = append(s.publishedMeta, w.Metadata)
	s.publishedExp = append(s.publishedExp, w.ExpectedVersion)
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
		out, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "body", ExpectedVersion: 5})
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
		out, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "a\nB\nc\n", ExpectedVersion: 5})
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
		out, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "a\nB\nc\n", ExpectedVersion: 5})
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
		out, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "x\ny\n", ExpectedVersion: 0})
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
		_, err := Candidate(t.Context(), &stubClient{}, Write{Path: "/p", Body: "b", ExpectedVersion: -1})
		if !errors.Is(err, ErrInvalidExpectedVersion) {
			t.Errorf("want ErrInvalidExpectedVersion, got %v", err)
		}
	})

	t.Run("metadata is forwarded on publish", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{{Status: "ok", Version: 6}},
		}
		meta := map[string]string{"agent": "test"}
		_, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "body", ExpectedVersion: 5, Metadata: meta})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := c.publishedMeta[0]["agent"]; got != "test" {
			t.Errorf("agent metadata lost: %q", got)
		}
	})

	t.Run("missing head version is rejected", func(t *testing.T) {
		// FetchCurrent returns ok with no version metadata. A candidate with
		// PublishAtVersion=0 would silently switch the agent's follow-up
		// publish to create-only mode, so we fail fast.
		c := &stubClient{
			versionedDocs: map[int]Doc{
				5: {Status: "ok", Body: "x\n", Version: 5},
			},
			currentDocs: []Doc{
				{Status: "ok", Body: "x\n", Version: 0},
			},
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
			},
		}
		_, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "x\nB\n", ExpectedVersion: 5})
		if err == nil {
			t.Fatal("expected error for missing head version")
		}
		if !strings.Contains(err.Error(), "version metadata") {
			t.Errorf("error should mention version metadata, got: %v", err)
		}
	})

	t.Run("base fetch failure propagates", func(t *testing.T) {
		c := &stubClient{
			publishResults: []PublishResult{
				{Status: "conflict", ServerVersion: 6},
			},
			fetchErr: errors.New("network down"),
		}
		_, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "body", ExpectedVersion: 5})
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
		out1, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "a\nB\nc\n", ExpectedVersion: 5})
		if err != nil {
			t.Fatalf("round 1: %v", err)
		}
		if out1.Status != OutcomeCandidate {
			t.Fatalf("round 1: want OutcomeCandidate, got %s", out1.Status)
		}

		// Agent's semantic verification is a no-op for this test.
		out2, err := Candidate(t.Context(), c, Write{Path: "/p", Body: out1.Body, ExpectedVersion: out1.PublishAtVersion})
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

// A lost response or a conflict against our own first attempt is a success
// when the head is exactly what was submitted at expected+1 (debt: self conflict).
func TestCandidateReconcilesItsOwnWrite(t *testing.T) {
	tests := []struct {
		name       string
		meta       map[string]string
		publishErr error
		publish    PublishResult
		head       Doc
		wantStatus OutcomeStatus
		wantErr    bool
	}{
		{name: "lost response, write landed", publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "mine", Version: 4}, wantStatus: OutcomeOK},
		{name: "lost response, write did not land", publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "old", Version: 3}, wantErr: true},
		{name: "lost response, someone else wrote", publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "theirs", Version: 4}, wantErr: true},
		{name: "conflict with our own attempt", publish: PublishResult{Status: statusConflict, ServerVersion: 4}, head: Doc{Status: statusOK, Body: "mine", Version: 4}, wantStatus: OutcomeOK},
		{name: "conflict with another writer", publish: PublishResult{Status: statusConflict, ServerVersion: 4}, head: Doc{Status: statusOK, Body: "theirs", Version: 4}, wantStatus: OutcomeCandidate},
		{name: "same body, our metadata landed", meta: map[string]string{"tags": "a,b"}, publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "mine", Version: 4, Metadata: map[string]string{"tags": "a,b", "version": "4"}}, wantStatus: OutcomeOK},
		{name: "same body, someone else's metadata", meta: map[string]string{"tags": "a,b"}, publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "mine", Version: 4, Metadata: map[string]string{"tags": "z"}}, wantErr: true},
		{name: "empty submitted value, key absent on head", meta: map[string]string{"tags": ""}, publishErr: errors.New("timeout"), head: Doc{Status: statusOK, Body: "mine", Version: 4, Metadata: map[string]string{"version": "4"}}, wantErr: true},
		{name: "conflict, same body, someone else's metadata", meta: map[string]string{"tags": "a,b"}, publish: PublishResult{Status: statusConflict, ServerVersion: 4}, head: Doc{Status: statusOK, Body: "mine", Version: 4}, wantStatus: OutcomeCandidate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &stubClient{
				versionedDocs:  map[int]Doc{3: {Status: statusOK, Body: "old", Version: 3}},
				currentDocs:    []Doc{tt.head},
				publishResults: []PublishResult{tt.publish},
				publishErrs:    []error{tt.publishErr},
			}
			out, err := Candidate(t.Context(), c, Write{Path: "/doc.md", Body: "mine", ExpectedVersion: 3, Metadata: tt.meta})
			if tt.wantErr != (err != nil) {
				t.Fatalf("Candidate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if out.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", out.Status, tt.wantStatus)
			}
			if tt.wantStatus == OutcomeOK && out.Publish.Version != 4 {
				t.Errorf("version = %d, want 4", out.Publish.Version)
			}
			if len(c.publishedBody) != 1 {
				t.Errorf("publish called %d times, want exactly 1", len(c.publishedBody))
			}
		})
	}
}

// Every call Candidate makes, the merge path's three included, runs under the
// caller's context, so a cancelled tool call stops instead of finishing alone.
func TestCandidatePassesItsContextToEveryCall(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "caller")
	c := &stubClient{
		publishResults: []PublishResult{{Status: "conflict"}},
		currentDocs:    []Doc{{Status: "ok", Body: "a\nb\nC\n", Version: 6}},
		versionedDocs:  map[int]Doc{5: {Status: "ok", Body: "a\nb\nc\n", Version: 5}},
	}
	out, err := Candidate(ctx, c, Write{Path: "/p", Body: "a\nB\nc\n", ExpectedVersion: 5})
	if err != nil || out.Status != OutcomeCandidate {
		t.Fatalf("Candidate = %+v, %v, want a merge candidate", out, err)
	}
	if len(c.contexts) != 3 {
		t.Fatalf("calls = %d, want publish, fetch current, fetch base", len(c.contexts))
	}
	for i, got := range c.contexts {
		if got.Value(key{}) != "caller" {
			t.Errorf("call %d ran under a context that is not the caller's", i)
		}
	}
}

// Without the version the writer started from there is nothing to merge
// against: it never existed, or retention pruned it. The conflict is the answer.
func TestCandidateWithoutABaseReportsTheConflict(t *testing.T) {
	conflict := PublishResult{Status: "conflict", ServerVersion: 6, Metadata: map[string]string{"server-version": "6"}}
	c := &stubClient{
		publishResults: []PublishResult{conflict},
		currentDocs:    []Doc{{Status: "ok", Body: "theirs", Version: 6}},
		versionedDocs:  map[int]Doc{}, // v9 answers not-found
	}
	out, err := Candidate(t.Context(), c, Write{Path: "/p", Body: "mine", ExpectedVersion: 9})
	if err != nil {
		t.Fatalf("err = %v, want the conflict as an outcome", err)
	}
	if out.Status != OutcomeOK || out.Publish.Status != "conflict" || out.Publish.ServerVersion != 6 {
		t.Errorf("outcome = %+v, want the server's conflict passed through", out)
	}
}
