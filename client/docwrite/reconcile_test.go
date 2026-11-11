package docwrite_test

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/fetchtest"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
)

const classifierStamp = "unchecked: test"

// storeServer is the stored document model behind the handler's metadata
// path: controls and stale stamps dropped, a classifier stamp set, PUBLISH
// typed (the store types an APPEND), reserved keys hidden from a FETCH.
type storeServer struct {
	store *versionStore
	lose  bool // the store runs each write, then its answer is lost
}

// newStoreServer holds one version per body, each published with meta.
func newStoreServer(t *testing.T, meta map[string]string, bodies ...string) *storeServer {
	t.Helper()
	s := &storeServer{store: newVersionStore()}
	for i, body := range bodies {
		s.put(t, protocol.VerbPublish, i, body, meta)
	}
	return s
}

// put is a write answered created: another writer's, or an earlier attempt of ours.
func (s *storeServer) put(t *testing.T, verb string, expected int, body string, meta map[string]string) {
	t.Helper()
	r, err := s.write(verb, fetch.WriteRequest{Path: "/doc.md", ExpectedVersion: expected, Body: body, Metadata: meta})
	if err != nil || r.Response.Status != protocol.StatusCreated {
		t.Fatalf("%s at v%d = %+v, %v", verb, expected, r.Response, err)
	}
}

func (s *storeServer) write(verb string, r fetch.WriteRequest) (fetch.Result, error) {
	meta := map[string]string{protocol.ClassifierKey: classifierStamp}
	for k, v := range r.Metadata {
		switch {
		case protocol.IsControlMetadataKey(k), protocol.IsServerStampedKey(k):
		case protocol.IsReservedMetadataKey(k):
			return fetch.Result{Response: protocol.Response{Status: protocol.StatusBadRequest}}, nil
		default:
			meta[k] = v
		}
	}
	var doc *storefmt.Document
	var err error
	if verb == protocol.VerbAppend {
		doc, err = s.store.Append(r.Path, r.ExpectedVersion, []byte(r.Body), meta)
	} else {
		doc, err = s.store.WriteVersion(r.Path, r.ExpectedVersion, []byte(r.Body), storefmt.ApplyOKFTypeDefault(r.Path, meta))
	}
	switch {
	case errors.Is(err, storefmt.ErrConflict):
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusConflict, Metadata: map[string]string{"server-version": strconv.Itoa(doc.Version)}}}, nil
	case err != nil:
		return fetch.Result{}, err
	}
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusCreated, Metadata: map[string]string{"version": strconv.Itoa(doc.Version)}}}, nil
}

func (s *storeServer) fetch(_ context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	path, version := r.Path, 0
	if i := strings.LastIndex(r.Path, "/v"); i > 0 {
		if n, err := strconv.Atoi(r.Path[i+2:]); err == nil {
			path, version = r.Path[:i], n
		}
	}
	doc, err := s.store.Get(path, version)
	if errors.Is(err, os.ErrNotExist) {
		return fetch.Result{Response: protocol.Response{Status: protocol.StatusNotFound}}, nil
	}
	if err != nil {
		return fetch.Result{}, err
	}
	meta := map[string]string{}
	for k, v := range doc.Metadata {
		if !protocol.IsReservedMetadataKey(k) && !protocol.IsControlMetadataKey(k) {
			meta[k] = v
		}
	}
	meta["version"] = strconv.Itoa(doc.Version)
	meta["modified"] = doc.Modified.Format(time.RFC3339)
	meta["etag"] = doc.ETag
	meta["content-hash"] = storefmt.ContentHash(doc.Content)
	if version > 0 {
		current, err := s.store.CurrentVersionResult(path)
		if err != nil {
			return fetch.Result{}, err
		}
		meta["current-version"] = strconv.Itoa(current)
	}
	return fetch.Result{Response: protocol.Response{Status: protocol.StatusOK, Body: string(doc.Content), Metadata: meta}}, nil
}

func (s *storeServer) backend() *fetchtest.Client {
	send := func(verb string) func(context.Context, fetch.WriteRequest) (fetch.Result, error) {
		return func(_ context.Context, r fetch.WriteRequest) (fetch.Result, error) {
			answer, err := s.write(verb, r)
			if err != nil || !s.lose {
				return answer, err
			}
			return fetch.Result{}, fetchtest.LostResponse()
		}
	}
	return &fetchtest.Client{PublishFn: send(protocol.VerbPublish), AppendFn: send(protocol.VerbAppend), FetchFn: s.fetch}
}

// The base every write here starts from: v3 "old", typed and retained.
var baseMeta = map[string]string{"tags": "x", "owner": "base", "type": "Note", "retention": "10"}

// PUBLISH replaces the whole map, so v4 is ours only when it holds exactly
// what our request stores: typed, tags as stored, server stamps aside. A
// subset match would take a competitor that kept a key we dropped for ours.
func TestPublishReconcilesAgainstWhatTheStoreKeeps(t *testing.T) {
	tests := []struct {
		name         string
		landed, sent map[string]string // sent nil: what landed
		ours         bool
	}{
		{name: "ours, tags as typed", landed: map[string]string{"tags": "a, b"}, ours: true},
		{name: "ours, type defaulted", landed: map[string]string{"tags": "a"}, ours: true},
		{name: "ours, empty values", landed: map[string]string{"tags": "", "description": ""}, ours: true},
		{name: "ours, a value kept with its spaces", landed: map[string]string{"tags": "a", "note": " spaced "}, ours: true},
		{name: "ours, retention pruned v1 and v2", landed: map[string]string{"tags": "a", "retention": "2"}, ours: true},
		{name: "ours, a stale classifier sent", landed: map[string]string{"tags": "a", protocol.ClassifierKey: "injection: stale"}, ours: true},
		{name: "theirs kept a key ours removed", landed: map[string]string{"tags": "a", "owner": "base"}, sent: map[string]string{"tags": "a"}},
		{name: "theirs kept a tag ours removed", landed: map[string]string{"tags": "a,x"}, sent: map[string]string{"tags": "a"}},
		{name: "theirs declared a type ours left to the default", landed: map[string]string{"tags": "a", "type": "Note"}, sent: map[string]string{"tags": "a"}},
		{name: "theirs kept the retention ours dropped", landed: map[string]string{"tags": "a", "retention": "10"}, sent: map[string]string{"tags": "a"}},
		{name: "theirs lacks our empty value", landed: map[string]string{"tags": "a"}, sent: map[string]string{"tags": "a", "description": ""}},
	}
	for _, tt := range tests {
		for _, lost := range []bool{true, false} {
			mode := "conflict"
			if lost {
				mode = "lost response"
			}
			t.Run(tt.name+"/"+mode, func(t *testing.T) {
				s := newStoreServer(t, baseMeta, "one", "two", "old")
				s.put(t, protocol.VerbPublish, 3, "mine", tt.landed)
				s.lose = lost
				backend := s.backend()
				sent := tt.sent
				if sent == nil {
					sent = tt.landed
				}
				got, err := doc(backend).Publish(t.Context(), docwrite.Write{Body: "mine", ExpectedVersion: 3, Metadata: sent}, merge.OnConflictMerge)
				switch {
				case tt.ours:
					if err != nil || !got.Reconciled || versionOf(got) != 4 {
						t.Fatalf("got %+v, %v; want our write found at v4", got, err)
					}
				case lost:
					if !errors.Is(err, protocol.ErrOutcomeUnknown) {
						t.Fatalf("got %+v, %v; want the unknown outcome to stand", got, err)
					}
				default:
					if err != nil || got.Reconciled || got.Candidate == nil {
						t.Fatalf("got %+v, %v; want a merge candidate", got, err)
					}
				}
				if len(backend.PublishCalls) != 1 {
					t.Errorf("publishes = %d, want exactly one: never resend", len(backend.PublishCalls))
				}
			})
		}
	}
}

// APPEND layers its keys over the base's, retention excepted, then types the
// result: v4 is ours only when it holds exactly that, over the joined body.
func TestAppendReconcilesAgainstWhatTheStoreKeeps(t *testing.T) {
	me := map[string]string{"agent": "me"}
	tests := []struct {
		name         string
		verb, body   string // what wrote v4
		landed, sent map[string]string
		ours         bool
	}{
		{name: "ours, base keys inherited, retention not", verb: protocol.VerbAppend, body: "more", landed: me, sent: me, ours: true},
		{name: "ours, tags as typed", verb: protocol.VerbAppend, body: "more", landed: map[string]string{"agent": "me", "tags": "y, z"}, sent: map[string]string{"agent": "me", "tags": "y, z"}, ours: true},
		{name: "ours, an empty value over an inherited one", verb: protocol.VerbAppend, body: "more", landed: map[string]string{"agent": "me", "owner": ""}, sent: map[string]string{"agent": "me", "owner": ""}, ours: true},
		{name: "ours, retention kept the base", verb: protocol.VerbAppend, body: "more", landed: map[string]string{"agent": "me", "retention": "2"}, sent: map[string]string{"agent": "me", "retention": "2"}, ours: true},
		// Nothing to compare against, so nothing is claimed.
		{name: "ours, retention pruned the base", verb: protocol.VerbAppend, body: "more", landed: map[string]string{"agent": "me", "retention": "1"}, sent: map[string]string{"agent": "me", "retention": "1"}},
		{name: "theirs added a key", verb: protocol.VerbAppend, body: "more", landed: map[string]string{"agent": "me", "extra": "y"}, sent: me},
		{name: "theirs is a publish without the inherited keys", verb: protocol.VerbPublish, body: "old\nmore", landed: me, sent: me},
		{name: "theirs is a publish that kept the base's retention", verb: protocol.VerbPublish, body: "old\nmore", landed: map[string]string{"tags": "x", "owner": "base", "type": "Note", "retention": "10", "agent": "me"}, sent: me},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStoreServer(t, baseMeta, "one", "two", "old")
			s.put(t, tt.verb, 3, tt.body, tt.landed)
			s.lose = true
			backend := s.backend()
			got, err := doc(backend).Append(t.Context(), docwrite.AppendRequest{Body: "more", ExpectedVersion: 3, Metadata: tt.sent})
			if tt.ours {
				if err != nil || !got.Reconciled || versionOf(got) != 4 {
					t.Fatalf("got %+v, %v; want our append found at v4", got, err)
				}
			} else if !errors.Is(err, protocol.ErrOutcomeUnknown) {
				t.Fatalf("got %+v, %v; want the unknown outcome to stand", got, err)
			}
			if len(backend.AppendCalls) != 1 {
				t.Errorf("appends = %d, want exactly one: never resend", len(backend.AppendCalls))
			}
		})
	}
}
