package docwrite_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

func doc(backend docwrite.Backend) *docwrite.Doc {
	return &docwrite.Doc{
		Backend: backend, Host: "host:6309", Path: "/doc.md", ReadToken: "read-token",
		Write: docwrite.SendOnce("write-token"),
	}
}

func versionOf(r docwrite.Result) int { //nolint:gocritic // a test reads one result
	v, _ := strconv.Atoi(r.Response.Metadata["version"]) // absent reads as 0, which no case wants
	return v
}

func answer(status string, meta map[string]string) func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
	return func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: status, Metadata: meta}}, nil
	}
}

var conflictAt6 = answer(protocol.StatusConflict, map[string]string{"server-version": "6"})

// history serves the head, at /doc.md and at its own version path as a server
// does, and base at /doc.md/v<baseVersion>; any other version is not found.
func history(baseVersion int, base string, head fetch.Result) func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
	// A head without a usable version has no version path to be served at.
	headPath := ""
	if v, err := strconv.Atoi(head.Response.Metadata["version"]); err == nil && v > 0 {
		headPath = protocol.VersionPath("/doc.md", v)
	}
	return func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		switch r.Path {
		case "/doc.md", headPath:
			return head, nil
		case protocol.VersionPath("/doc.md", baseVersion):
			return fetchtest.Head(base, baseVersion, nil), nil
		}
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
}

func TestPublishModes(t *testing.T) {
	backend := &fetchtest.Client{PublishFn: conflictAt6, FetchFn: history(5, "a\nb\nc\n", fetchtest.Head("a\nb\nC\n", 6, nil))}
	w := docwrite.Write{Body: "a\nB\nc\n", ExpectedVersion: 5, Metadata: map[string]string{"tags": "x"}}

	merged, err := doc(backend).Publish(t.Context(), w, merge.OnConflictMerge)
	if err != nil || merged.Candidate == nil {
		t.Fatalf("merge mode = %+v, %v, want a candidate", merged, err)
	}
	if c := merged.Candidate; c.Body != "a\nB\nC\n" || c.HasMarkers || c.BaseVersion != 5 || c.TheirVersion != 6 || c.PublishAtVersion != 6 {
		t.Errorf("candidate = %+v", c)
	}
	failed, err := doc(backend).Publish(t.Context(), w, merge.OnConflictFail)
	if err != nil || failed.Candidate != nil || failed.Response.Status != protocol.StatusConflict || failed.Response.Metadata["server-version"] != "6" {
		t.Fatalf("fail mode = %+v, %v, want the conflict reported as is", failed, err)
	}
	sent := backend.PublishCalls[0]
	if sent.Host != "host:6309" || sent.Path != "/doc.md" || sent.Token != "write-token" || sent.Metadata["tags"] != "x" || backend.FetchCalls[0].Token != "read-token" {
		t.Errorf("publish = %+v, first read = %+v", sent, backend.FetchCalls[0])
	}
	// A candidate is offered, never published: one attempt per call.
	if len(backend.PublishCalls) != 2 {
		t.Errorf("publishes = %d, want one per call", len(backend.PublishCalls))
	}
}

func TestPublishCandidates(t *testing.T) {
	tests := []struct {
		name        string
		w           docwrite.Write
		fetch       func(context.Context, fetch.FetchRequest) (fetch.Result, error)
		wantBody    string
		wantMarkers bool
		wantErr     string
	}{
		{name: "overlapping edits keep both sides in markers",
			w: docwrite.Write{Body: "a\nMINE\nc\n", ExpectedVersion: 5}, fetch: history(5, "a\nb\nc\n", fetchtest.Head("a\nTHEIRS\nc\n", 6, nil)),
			wantBody: "a\n<<<<<<< ours\nMINE\n=======\nTHEIRS\n>>>>>>> theirs\nc\n", wantMarkers: true},
		{name: "create only conflict merges against an empty base",
			w: docwrite.Write{Body: "mine\n", ExpectedVersion: 0}, fetch: history(0, "", fetchtest.Head("theirs\n", 1, nil)),
			wantBody: "<<<<<<< ours\nmine\n=======\ntheirs\n>>>>>>> theirs\n", wantMarkers: true},
		{name: "a head without a version cannot be republished at",
			w: docwrite.Write{Body: "x", ExpectedVersion: 5}, fetch: history(5, "base", fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "theirs"}}),
			wantErr: "fetch current: missing or invalid version metadata"},
		{name: "a head that cannot be read",
			w: docwrite.Write{Body: "x", ExpectedVersion: 5}, fetch: history(5, "base", fetch.Result{Response: protocol.Response{Status: protocol.StatusServerError}}),
			wantErr: "fetch current: status server-error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fetchtest.Client{PublishFn: conflictAt6, FetchFn: tt.fetch}
			got, err := doc(backend).Publish(t.Context(), tt.w, merge.OnConflictMerge)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.Candidate == nil || got.Candidate.Body != tt.wantBody || got.Candidate.HasMarkers != tt.wantMarkers {
				t.Fatalf("got %+v, %v\nwant body %q markers=%v", got.Candidate, err, tt.wantBody, tt.wantMarkers)
			}
		})
	}
}

func TestPublishBaseFetchFailureIsTheError(t *testing.T) {
	boom := errors.New("boom")
	backend := &fetchtest.Client{PublishFn: conflictAt6, FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		if r.Path != "/doc.md" {
			return fetch.Result{}, boom
		}
		return fetchtest.Head("theirs", 6, nil), nil
	}}
	_, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 5}, merge.OnConflictMerge)
	if !errors.Is(err, boom) || err.Error() != "fetch base v5: boom" {
		t.Errorf("err = %v", err)
	}
}

// Without the version the writer started from there is nothing to merge
// against: it never existed, or retention pruned it. The conflict is the answer.
func TestPublishWithoutABaseReportsTheConflict(t *testing.T) {
	backend := &fetchtest.Client{PublishFn: conflictAt6, FetchFn: history(5, "base", fetchtest.Head("theirs", 6, nil))}
	got, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 9}, merge.OnConflictMerge)
	if err != nil || got.Candidate != nil || got.Response.Status != protocol.StatusConflict || got.Response.Metadata["server-version"] != "6" {
		t.Errorf("got %+v, %v, want the server's conflict passed through", got, err)
	}
}

func TestNegativeVersionsAreRefusedUnsent(t *testing.T) {
	backend := &fetchtest.Client{}
	_, errPublish := doc(backend).Publish(t.Context(), docwrite.Write{ExpectedVersion: -1}, merge.OnConflictMerge)
	_, errAppend := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "x", ExpectedVersion: -1})
	if !errors.Is(errPublish, docwrite.ErrInvalidExpectedVersion) || !errors.Is(errAppend, docwrite.ErrInvalidExpectedVersion) {
		t.Errorf("publish err = %v, append err = %v", errPublish, errAppend)
	}
	if len(backend.PublishCalls)+len(backend.AppendCalls) != 0 {
		t.Error("a refused write reached the backend")
	}
}

// An agent that republishes the reviewed candidate at the version it names succeeds.
func TestRepublishingTheCandidateLands(t *testing.T) {
	published := 0
	backend := &fetchtest.Client{
		PublishFn: func(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			published++
			if published == 1 {
				return conflictAt6(ctx, r)
			}
			return answer(protocol.StatusOK, map[string]string{"version": "7"})(ctx, r)
		},
		FetchFn: history(5, "a\nb\nc\n", fetchtest.Head("a\nb\nC\n", 6, nil)),
		// The fake checks versions like the server: it holds v6 when we republish.
		Published: map[string]fetch.Result{"host:6309/doc.md": fetchtest.Head("a\nb\nC\n", 6, nil)},
	}
	first, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "a\nB\nc\n", ExpectedVersion: 5}, merge.OnConflictMerge)
	if err != nil || first.Candidate == nil {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: first.Candidate.Body, ExpectedVersion: first.Candidate.PublishAtVersion}, merge.OnConflictMerge)
	if err != nil || second.Candidate != nil || versionOf(second) != 7 || backend.PublishCalls[1].ExpectedVersion != 6 {
		t.Errorf("second = %+v, %v, sent = %+v", second, err, backend.PublishCalls[1])
	}
}

// What counts as "our write is at the head": the body at the next version under
// every metadata key that was sent. Both modes and the conflict path agree.
func TestPublishRecognizesItsOwnWrite(t *testing.T) {
	tags := map[string]string{"tags": "a,b"}
	tests := []struct {
		name      string
		meta      map[string]string
		publish   func(context.Context, fetch.WriteRequest) (fetch.Result, error)
		head      fetch.Result
		wantOurs  bool
		wantError bool
	}{
		{name: "lost response, write landed", head: fetchtest.Head("mine", 4, nil), wantOurs: true},
		{name: "lost response, write did not land", head: fetchtest.Head("old", 3, nil), wantError: true},
		{name: "lost response, someone else wrote", head: fetchtest.Head("theirs", 4, nil), wantError: true},
		{name: "lost response, our metadata landed", meta: tags, head: fetchtest.Head("mine", 4, tags), wantOurs: true},
		{name: "lost response, someone else's metadata", meta: tags, head: fetchtest.Head("mine", 4, map[string]string{"tags": "z"}), wantError: true},
		{name: "lost response, empty value sent, key absent", meta: map[string]string{"tags": ""}, head: fetchtest.Head("mine", 4, nil), wantError: true},
		{name: "conflict with our own attempt", publish: conflictAt6, head: fetchtest.Head("mine", 4, nil), wantOurs: true},
		{name: "conflict with another writer", publish: conflictAt6, head: fetchtest.Head("theirs", 4, nil)},
		{name: "conflict, same body, someone else's metadata", meta: tags, publish: conflictAt6, head: fetchtest.Head("mine", 4, nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publish := tt.publish
			if publish == nil {
				publish = func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
					return fetch.Result{}, fetchtest.LostResponse()
				}
			}
			backend := &fetchtest.Client{PublishFn: publish, FetchFn: history(3, "old", tt.head)}
			got, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 3, Metadata: tt.meta}, merge.OnConflictMerge)
			switch {
			case tt.wantError:
				if !errors.Is(err, protocol.ErrOutcomeUnknown) {
					t.Fatalf("err = %v, want the unknown outcome to stand", err)
				}
			case tt.wantOurs:
				if err != nil || !got.Reconciled || got.Response.Status != protocol.StatusOK || versionOf(got) != 4 {
					t.Fatalf("got %+v, %v, want our write found at v4", got, err)
				}
			default:
				if err != nil || got.Candidate == nil {
					t.Fatalf("got %+v, %v, want a candidate", got, err)
				}
			}
			if len(backend.PublishCalls) != 1 {
				t.Errorf("publishes = %d, want exactly one: never resend", len(backend.PublishCalls))
			}
		})
	}
}

// A write whose response was lost may have landed: look at the head, never resend.
func TestWritesReconcileAnUnknownOutcome(t *testing.T) {
	meta := map[string]string{"agent": "me"}
	publish := func(d *docwrite.Doc) (docwrite.Result, error) {
		return d.Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 3, Metadata: meta}, merge.OnConflictFail)
	}
	appendMore := func(d *docwrite.Doc) (docwrite.Result, error) {
		return d.Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
	}
	archive := func(d *docwrite.Doc) (docwrite.Result, error) { return d.Archive(t.Context()) }
	archived := fetchtest.Archived()
	tests := []struct {
		name        string
		call        func(*docwrite.Doc) (docwrite.Result, error)
		head        fetch.Result
		landed      bool
		wantVersion string
	}{
		{"publish landed", publish, fetchtest.Head("mine", 4, meta), true, "4"},
		{"publish not landed", publish, fetchtest.Head("theirs", 4, nil), false, ""},
		{"append landed", appendMore, fetchtest.Head("old\nmore", 4, meta), true, "4"},
		{"append by someone else", appendMore, fetchtest.Head("old\nmore", 4, map[string]string{"agent": "other"}), false, ""},
		{"append without the key it sent", appendMore, fetchtest.Head("old\nmore", 4, nil), false, ""},
		{"archive landed", archive, archived, true, ""},
		{"archive still live", archive, fetchtest.Head("live", 3, nil), false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lostWrite := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
				return fetch.Result{}, fetchtest.LostResponse()
			}
			backend := &fetchtest.Client{
				PublishFn: lostWrite, AppendFn: lostWrite,
				ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) {
					return fetch.Result{}, fetchtest.LostResponse()
				},
				FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
					if r.Path == "/doc.md/v3" {
						return fetchtest.Head("old", 3, nil), nil // the base every write here started from
					}
					return tt.head, nil
				},
			}
			got, err := tt.call(doc(backend))
			if !tt.landed {
				if !errors.Is(err, protocol.ErrOutcomeUnknown) {
					t.Fatalf("err = %v, want the unknown outcome to stand", err)
				}
			} else if err != nil || !got.Reconciled || got.Response.Status != protocol.StatusOK {
				t.Fatalf("got %+v, %v; want ok reconciled", got, err)
			} else if version, claimed := got.Response.Metadata["version"]; claimed != (tt.wantVersion != "") || version != tt.wantVersion {
				// An archived head has no version: the result must not invent one.
				t.Errorf("version = %q (claimed %v), want %q", version, claimed, tt.wantVersion)
			}
			if n := len(backend.PublishCalls) + len(backend.AppendCalls) + len(backend.ArchiveCalls); n != 1 {
				t.Errorf("writes sent = %d, want exactly one", n)
			}
		})
	}
}

// A write that never left has a known outcome and costs no probe, in either mode.
func TestDefiniteFailuresDoNotProbe(t *testing.T) {
	refused := errors.New("dial refused")
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		AppendFn:  func(context.Context, fetch.WriteRequest) (fetch.Result, error) { return fetch.Result{}, refused },
		ArchiveFn: func(context.Context, fetch.ArchiveRequest) (fetch.Result, error) { return fetch.Result{}, refused },
	}
	d := doc(backend)
	_, errMerge := d.Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 3}, merge.OnConflictMerge)
	_, errFail := d.Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 3}, merge.OnConflictFail)
	_, errAppend := d.Append(t.Context(), docwrite.AppendRequest{Body: "x", ExpectedVersion: 3})
	_, errArchive := d.Archive(t.Context())
	for name, err := range map[string]error{"publish merge": errMerge, "publish fail": errFail, "append": errAppend, "archive": errArchive} {
		if !errors.Is(err, refused) {
			t.Errorf("%s err = %v", name, err)
		}
	}
	// Tool text in merge mode has always named the step.
	if errMerge.Error() != "publish: dial refused" || errFail.Error() != "dial refused" {
		t.Errorf("merge = %q, fail = %q", errMerge, errFail)
	}
	if len(backend.FetchCalls) != 0 {
		t.Errorf("head probes = %+v, want none", backend.FetchCalls)
	}
}

// Every call a publish makes, the conflict path's two reads included, runs
// under the caller's context, so a cancelled tool call stops.
func TestPublishPassesItsContextToEveryCall(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(t.Context(), key{}, "caller")
	calls := 0
	check := func(ctx context.Context) {
		calls++
		if ctx.Value(key{}) != "caller" {
			t.Errorf("call %d ran under a context that is not the caller's", calls)
		}
	}
	inner := history(5, "a\nb\nc\n", fetchtest.Head("a\nb\nC\n", 6, nil))
	backend := &fetchtest.Client{
		PublishFn: func(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			check(ctx)
			return conflictAt6(ctx, r)
		},
		FetchFn: func(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
			check(ctx)
			return inner(ctx, r)
		},
	}
	if got, err := doc(backend).Publish(ctx, docwrite.Write{Body: "a\nB\nc\n", ExpectedVersion: 5}, merge.OnConflictMerge); err != nil || got.Candidate == nil {
		t.Fatalf("Publish = %+v, %v", got, err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want publish, fetch current, fetch base", calls)
	}
}

func TestAppendResolvesTheCurrentVersion(t *testing.T) {
	backend := &fetchtest.Client{
		VersionsFn: func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
			return fetchtest.Versions("/doc.md", 7), nil
		},
	}
	got, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more"})
	if err != nil || got.Response.Status != protocol.StatusOK {
		t.Fatalf("Append = %+v, %v", got, err)
	}
	if backend.AppendCalls[0].ExpectedVersion != 7 || backend.AppendCalls[0].Token != "write-token" || backend.VersionsCalls[0].Token != "read-token" {
		t.Errorf("append = %+v, versions = %+v", backend.AppendCalls[0], backend.VersionsCalls[0])
	}

	backend.VersionsFn = func(context.Context, fetch.VersionsRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	var unresolved *docwrite.VersionError
	if _, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more"}); !errors.As(err, &unresolved) || unresolved.Error() != "could not resolve version: not-found" {
		t.Errorf("err = %v, want a VersionError naming the status", err)
	}
}

// A refusal's body is the server saying what to fix; both modes must keep it.
func TestPublishKeepsTheServersExplanation(t *testing.T) {
	const why = "\n# Bad Request\n\npolicy: missing-tags\n"
	backend := &fetchtest.Client{PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest, Body: why}}, nil
	}}
	for _, mode := range []string{merge.OnConflictMerge, merge.OnConflictFail} {
		got, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 1}, mode)
		if err != nil || got.Response.Status != protocol.StatusBadRequest || got.Response.Body != why {
			t.Errorf("%s mode = %+v, %v, want the refusal with its body", mode, got.Response, err)
		}
	}
}

// An answered write is reported as answered, whatever its metadata looks like:
// calling it a failure would invite a resend of a write that landed.
func TestAnAnsweredWriteIsPassedOnAsWritten(t *testing.T) {
	backend := &fetchtest.Client{PublishFn: answer(protocol.StatusOK, map[string]string{"version": "seven"})}
	got, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 1}, merge.OnConflictFail)
	if err != nil || got.Response.Metadata["version"] != "seven" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// The head is different: its version decides whether a write landed.
func TestAHeadWithAMalformedVersionSettlesNothing(t *testing.T) {
	backend := &fetchtest.Client{
		PublishFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{}, fetchtest.LostResponse()
		},
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: "x", Metadata: map[string]string{"version": "seven"}}}, nil
		},
	}
	if _, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "x", ExpectedVersion: 1}, merge.OnConflictFail); !errors.Is(err, protocol.ErrOutcomeUnknown) {
		t.Errorf("err = %v, want the unknown outcome to stand", err)
	}
}

// A probe that fails settles nothing, but it is not swallowed: the caller
// learns the outcome is unknown and why the look at the head did not help.
func TestAFailedHeadProbeIsReported(t *testing.T) {
	probeFailed := errors.New("dial refused")
	backend := &fetchtest.Client{
		AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{}, fetchtest.LostResponse()
		},
		FetchFn: func(context.Context, fetch.FetchRequest) (fetch.Result, error) { return fetch.Result{}, probeFailed },
	}
	_, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "x", ExpectedVersion: 3})
	if !errors.Is(err, protocol.ErrOutcomeUnknown) || !errors.Is(err, probeFailed) {
		t.Fatalf("err = %v, want the unknown outcome and the probe failure", err)
	}
	if want := "read response: request sent but outcome unknown; reconcile: dial refused"; err.Error() != want {
		t.Errorf("err = %q\nwant  %q", err, want)
	}
}

// Someone else may write between our lost response and our look. The version
// our write would have created is immutable, so that is where to look: the
// head having moved on must not turn a landed write into an unknown one.
func TestReconcileLooksAtTheVersionItWroteNotTheHead(t *testing.T) {
	meta := map[string]string{"agent": "me"}
	lostWrite := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{}, fetchtest.LostResponse()
	}
	tests := []struct {
		name string
		ours fetch.Result // what /doc.md/v4 holds
		call func(*docwrite.Doc) (docwrite.Result, error)
		want bool
	}{
		{"publish landed, head moved on", fetchtest.Head("mine", 4, meta), func(d *docwrite.Doc) (docwrite.Result, error) {
			return d.Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 3, Metadata: meta}, merge.OnConflictFail)
		}, true},
		{"append landed, head moved on", fetchtest.Head("old\nmore", 4, meta), func(d *docwrite.Doc) (docwrite.Result, error) {
			return d.Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
		}, true},
		{"v4 is someone else's", fetchtest.Head("theirs", 4, nil), func(d *docwrite.Doc) (docwrite.Result, error) {
			return d.Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 3, Metadata: meta}, merge.OnConflictFail)
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fetchtest.Client{
				PublishFn: lostWrite, AppendFn: lostWrite,
				FetchFn: func(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
					switch r.Path {
					case "/doc.md/v4":
						return tt.ours, nil
					case "/doc.md/v3":
						return fetchtest.Head("old", 3, nil), nil
					}
					return fetchtest.Head("a later writer's", 5, nil), nil
				},
			}
			got, err := tt.call(doc(backend))
			if !tt.want {
				if !errors.Is(err, protocol.ErrOutcomeUnknown) {
					t.Fatalf("err = %v, want the unknown outcome to stand", err)
				}
				return
			}
			if err != nil || !got.Reconciled || versionOf(got) != 4 {
				t.Fatalf("got %+v, %v; want our write found at v4", got, err)
			}
			// The version written, then for an append its base. Never the head, never more.
			wantReads := []string{"/doc.md/v4", "/doc.md/v3"}
			if len(backend.FetchCalls) > len(wantReads) {
				t.Fatalf("reads = %+v, want at most %v", backend.FetchCalls, wantReads)
			}
			for i, read := range backend.FetchCalls {
				if read.Path != wantReads[i] {
					t.Errorf("read %d = %s, want %s", i, read.Path, wantReads[i])
				}
			}
		})
	}
}

// An APPEND is known by the whole document it would have produced, base plus
// addition as the protocol joins them. A competing write at the same version
// may end in the same words under the same agent; that is not our append.
func TestAppendIsRecognizedByTheWholeDocumentNotItsSuffix(t *testing.T) {
	meta := map[string]string{"agent": "me"}
	lost := func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return fetch.Result{}, fetchtest.LostResponse()
	}
	appendMore := func(fetchFn func(context.Context, fetch.FetchRequest) (fetch.Result, error)) (docwrite.Result, error) {
		backend := &fetchtest.Client{AppendFn: lost, FetchFn: fetchFn}
		return doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
	}

	got, err := appendMore(fetchtest.History("/doc.md", meta, "a", "b", "old", "old\nmore"))
	if err != nil || !got.Reconciled || versionOf(got) != 4 {
		t.Errorf("ours = %+v, %v; want it found at v4", got, err)
	}
	// The base already ends in a newline: the protocol adds none.
	got, err = appendMore(fetchtest.History("/doc.md", meta, "a", "b", "old\n", "old\nmore"))
	if err != nil || !got.Reconciled {
		t.Errorf("base with a trailing newline = %+v, %v", got, err)
	}
	if _, err := appendMore(fetchtest.History("/doc.md", meta, "a", "b", "old", "someone rewrote it\nmore")); !errors.Is(err, protocol.ErrOutcomeUnknown) {
		t.Errorf("a competing write ending in the same words: err = %v, want the unknown outcome to stand", err)
	}
	// Retention pruned the base: nothing to compare against, so nothing is claimed.
	pruned := func(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
		if r.Path == "/doc.md/v3" {
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
		}
		return fetchtest.History("/doc.md", meta, "a", "b", "old", "old\nmore")(ctx, r)
	}
	if _, err := appendMore(pruned); !errors.Is(err, protocol.ErrOutcomeUnknown) {
		t.Errorf("pruned base: err = %v, want the unknown outcome to stand", err)
	}
}

// An addition that cannot be joined to the base cannot have been accepted. Why
// the comparison stopped is said beside the unknown outcome, not dropped.
func TestAnAppendThatCannotBeJoinedIsReported(t *testing.T) {
	meta := map[string]string{"agent": "me"}
	full := strings.Repeat("a", protocol.MaxBodyLength)
	backend := &fetchtest.Client{
		AppendFn: func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
			return fetch.Result{}, fetchtest.LostResponse()
		},
		FetchFn: fetchtest.History("/doc.md", meta, "a", "b", full, "someone else's, ending in more"),
	}
	_, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: meta})
	if !errors.Is(err, protocol.ErrOutcomeUnknown) || !errors.Is(err, storefmt.ErrSizeLimit) {
		t.Errorf("err = %v, want the unknown outcome and the size limit", err)
	}
}
