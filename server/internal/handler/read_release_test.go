package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/auth"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// writeProbe is a peer's stream: on its first write it reports whether a
// commit can take the store, which it cannot while a read view is open.
type writeProbe struct {
	bytes.Buffer
	store     DocumentStore
	commitErr error
	probed    bool
}

func (p *writeProbe) Write(b []byte) (int, error) {
	if !p.probed {
		p.probed = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, p.commitErr = p.store.Publish(ctx, storagebackend.WriteRequest{Path: "/during.md", ExpectedVersion: -1, Content: []byte("# During\n")})
	}
	return p.Buffer.Write(b)
}

// A response is written after the view closes: a peer that stops reading
// must not hold the file store's read lock against every writer.
func TestReadResponseIsWrittenAfterTheViewCloses(t *testing.T) {
	b := fileBackend(t)
	seedBackend(t, b, map[string]string{"hello.md": "# Hello\n"})
	h := newHandler(b, nil)
	for _, request := range []string{"FETCH /hello.md\n", "LIST /\n", "VERSIONS /hello.md\n", lookupReq("/", "query: hello")} {
		out := &writeProbe{store: b.Store}
		req, err := protocol.ParseRequest(strings.NewReader(request))
		if err != nil {
			t.Fatal(err)
		}
		h.Serve(context.Background(), out, req)
		if !out.probed {
			t.Fatalf("%q wrote no response", request)
		}
		if out.commitErr != nil && !errors.Is(out.commitErr, storefmt.ErrNotModified) {
			t.Errorf("%q: commit during the response write = %v; the view was still open", request, out.commitErr)
		}
		if resp, err := protocol.ParseResponse(&out.Buffer); err != nil || resp.Status != protocol.StatusOK {
			t.Errorf("%q: response = %+v, %v", request, resp.Status, err)
		}
	}
}

// lookupOptions records the options the handler's lookups reach the store with.
type lookupOptions struct {
	DocumentStore
	opts catalog.Options
}

func (s *lookupOptions) OpenReadView(ctx context.Context) (storagebackend.ReadView, error) {
	view, err := s.DocumentStore.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return &lookupOptionsView{ReadView: view, store: s}, nil
}

type lookupOptionsView struct {
	storagebackend.ReadView
	store *lookupOptions
}

func (v *lookupOptionsView) Lookup(ctx context.Context, query string, opts catalog.Options) ([]catalog.Result, error) {
	v.store.opts = opts
	return v.ReadView.Lookup(ctx, query, opts)
}

func protectedLookupHandler(t *testing.T, logs *bytes.Buffer, private int) (h *Handler, secret string) {
	t.Helper()
	secret = "read-secret"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret): {Paths: []string{"/private/**"}, Operations: []string{"read"}},
	})
	b := fileBackend(t)
	docs := make([]seedDoc, 0, 1+private)
	docs = append(docs, seedDoc{"/public/doc.md", map[string]string{"tags": "auth", "importance": "0.1"}})
	for i := range private {
		docs = append(docs, seedDoc{fmt.Sprintf("/private/dir/s%d.md", i), map[string]string{"tags": "auth", "importance": "0.9"}})
	}
	h = lookupHandler(t, b, docs...)
	h.getTokenStore = func() *auth.TokenStore { return ts }
	if logs != nil {
		h.logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	return h, secret
}

// Rows the requester may not read are dropped before the cap, so they never
// displace a readable match, and a denied row is not a log line.
func TestHandleLookupCapCountsReadableRowsOnly(t *testing.T) {
	var logs bytes.Buffer
	h, _ := protectedLookupHandler(t, &logs, 3)
	asked := &lookupOptions{DocumentStore: h.store}
	h.store = asked
	// Three protected rows outrank the public one and would fill the cap.
	resp := sendLookup(t, h, lookupReq("/", "query: auth", "limit: 2"))
	if asked.opts.Max != 2 || asked.opts.Skip == nil || !asked.opts.Skip("/private/dir/s0.md") || asked.opts.Skip("/public/doc.md") {
		t.Errorf("store lookup options = max %d, skip set %t; want the limit and a skip of unreadable paths", asked.opts.Max, asked.opts.Skip != nil)
	}
	if resp.Metadata["matches"] != "1" || !strings.Contains(resp.Body, "/public/doc.md") {
		t.Errorf("anonymous lookup = matches %q\n%s\nwant the public document", resp.Metadata["matches"], resp.Body)
	}
	if strings.Contains(resp.Body, "/private/") {
		t.Errorf("protected rows leaked:\n%s", resp.Body)
	}
	if strings.Contains(logs.String(), "authorization failed") || strings.Contains(logs.String(), "/private/") {
		t.Errorf("denied rows were logged:\n%s", logs.String())
	}
}

// A scope the requester may not read answers the same whether or not it
// exists; with the token, a missing scope is not-found as ever.
func TestHandleLookupScopeIsNoExistenceOracle(t *testing.T) {
	h, secret := protectedLookupHandler(t, nil, 1)
	exists := sendLookup(t, h, lookupReq("/private/dir/", "query: auth"))
	missing := sendLookup(t, h, lookupReq("/private/nope/", "query: auth"))
	if exists.Status != protocol.StatusOK || missing.Status != exists.Status || missing.Body != strings.ReplaceAll(exists.Body, "/private/dir", "/private/nope") {
		t.Errorf("anonymous lookups differ:\nexisting: %s\n%s\nmissing: %s\n%s", exists.Status, exists.Body, missing.Status, missing.Body)
	}
	if exists.Metadata["matches"] != "0" {
		t.Errorf("anonymous matches under a protected scope = %q, want 0", exists.Metadata["matches"])
	}
	if resp := sendLookup(t, h, lookupReq("/private/nope/", "query: auth", "auth: "+secret)); resp.Status != protocol.StatusNotFound {
		t.Errorf("authorized lookup of a missing scope = %q, want not-found", resp.Status)
	}
	if resp := sendLookup(t, h, lookupReq("/private/dir/", "query: auth", "auth: "+secret)); resp.Metadata["matches"] != "1" {
		t.Errorf("authorized lookup of the scope = matches %q, want 1", resp.Metadata["matches"])
	}
	if resp := sendLookup(t, h, lookupReq("/nope/", "query: auth")); resp.Status != protocol.StatusNotFound {
		t.Errorf("lookup of a missing public scope = %q, want not-found", resp.Status)
	}
}

// notingStore is a gated store as the handler sees one: it records the
// sender it was told about and answers with a note.
type notingStore struct {
	DocumentStore
	readable func(string) bool
}

func (s *notingStore) PublishNoted(ctx context.Context, req storagebackend.WriteRequest, sender storagebackend.Sender) (*storefmt.Document, []string, error) {
	s.readable = sender.Readable
	doc, err := s.Publish(ctx, req)
	return doc, []string{"noted by the gate"}, err
}

func (s *notingStore) AppendNoted(ctx context.Context, req storagebackend.WriteRequest, sender storagebackend.Sender) (*storefmt.Document, []string, error) {
	s.readable = sender.Readable
	doc, err := s.Append(ctx, req)
	return doc, []string{"noted by the gate"}, err
}

// The gate's notes and the sender's read check travel beside the storage
// request, not in it: a gated store gets both, a bare one neither.
func TestWriteNotesComeFromANotingStore(t *testing.T) {
	const secret = "writer"
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(secret):   {Paths: []string{"/**"}, Operations: []string{"publish"}},
		protocol.HashToken("reader"): {Paths: []string{"/private/**"}, Operations: []string{"read"}},
	})
	b := fileBackend(t)
	bare := newHandler(b, ts)
	if resp := sendLookup(t, bare, "PUBLISH /bare.md\n---\nauth: "+secret+"\n---\n# Bare\n"); resp.Status != protocol.StatusCreated || resp.Metadata["warning"] != "" {
		t.Fatalf("bare store publish = %s, warning %q", resp.Status, resp.Metadata["warning"])
	}
	gated := &notingStore{DocumentStore: b.Store}
	h := newHandler(b, ts)
	h.store = gated
	for _, request := range []string{
		"PUBLISH /gated.md\n---\nauth: " + secret + "\n---\n# Gated\n",
		"APPEND /gated.md\n---\nauth: " + secret + "\nexpected-version: 1\n---\nmore\n",
	} {
		resp := sendLookup(t, h, request)
		if resp.Status != protocol.StatusCreated || resp.Metadata["warning"] != "noted by the gate" {
			t.Errorf("%q = %s, warning %q; want the gate's note", strings.SplitN(request, "\n", 2)[0], resp.Status, resp.Metadata["warning"])
		}
		if gated.readable == nil || gated.readable("/private/x.md") || !gated.readable("/public/x.md") {
			t.Errorf("%q: the gate was not told what the sender may read", strings.SplitN(request, "\n", 2)[0])
		}
		gated.readable = nil
	}
}
