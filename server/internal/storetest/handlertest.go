package storetest

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/auth"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// HandlerToken is the write token NewHandler installs; it may publish anywhere.
const HandlerToken = "storetest-write-token"

// NewHandler wires a backend into a Handler the way main.go does for that
// backend: the store serves documents and the catalog serves LOOKUP.
func NewHandler(b LookupBackend) *handler.Handler {
	ts := auth.NewTokenStore(map[string]auth.Token{
		protocol.HashToken(HandlerToken): {Paths: []string{"/**"}, Operations: []string{"publish"}},
	})
	return &handler.Handler{
		Store:         b.Store,
		Catalog:       b.Catalog,
		GetTokenStore: func() *auth.TokenStore { return ts },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

type mockStream struct {
	io.Reader
	output bytes.Buffer
}

func (m *mockStream) Write(p []byte) (int, error) { return m.output.Write(p) }
func (m *mockStream) Close() error                { return nil }

// Send drives one request through the handler and parses the response.
func Send(t testing.TB, h *handler.Handler, req protocol.Request) protocol.Response {
	t.Helper()
	var wire bytes.Buffer
	if _, err := req.WriteTo(&wire); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return SendRaw(t, h, wire.String())
}

// SendRaw drives one wire-format request through the handler.
func SendRaw(t testing.TB, h *handler.Handler, wire string) protocol.Response {
	t.Helper()
	stream := &mockStream{Reader: strings.NewReader(wire)}
	h.HandleStream(stream)
	resp, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	return resp
}

// request builds a request; meta may be nil. Write verbs get the token.
func request(verb, path string, meta map[string]string, body string) protocol.Request {
	m := make(map[string]string, len(meta)+1)
	maps.Copy(m, meta)
	switch verb {
	case protocol.VerbPublish, protocol.VerbAppend, protocol.VerbArchive:
		m["auth"] = HandlerToken
	}
	return protocol.Request{Verb: verb, Path: path, Metadata: m, Body: body}
}

// RunHandlerDifferential is the protocol-level twin of RunDifferential: the
// same seeded op sequence is sent as requests to a Handler over each backend
// and every response, plus periodic read sweeps, must match.
func RunHandlerDifferential(t *testing.T, ref, cand LookupFactory, cfg DifferentialConfig) {
	// Fewer seeds than the store differential: each op here is a full request
	// round trip and the store suite already covers state parity in depth.
	runSeeds(t, cfg, []int64{1, 2, 3, 5}, func(t *testing.T, seed int64) {
		RunHandlerDifferentialSeed(t, ref(t), cand(t), seed, cfg)
	})
}

// RunHandlerDifferentialSeed runs one seeded sequence; exposed for fuzzing.
func RunHandlerDifferentialSeed(t *testing.T, ref, cand LookupBackend, seed int64, cfg DifferentialConfig) {
	t.Helper()
	d := &handlerDifferential{diffRun: newDiffRun(t, seed), ref: ref, cand: cand, refH: NewHandler(ref), candH: NewHandler(cand)}
	d.run(cfg.Ops, d.apply, d.compareSnapshots)
}

type handlerDifferential struct {
	*diffRun
	ref, cand   LookupBackend
	refH, candH *handler.Handler
}

func (d *handlerDifferential) apply(o op) {
	cur, ok := d.currentBoth(d.ref, d.cand, o.path)
	if !ok {
		return
	}
	req := requestFor(o, cur)
	refRes := normalize(Send(d.t, d.refH, req))
	candRes := normalize(Send(d.t, d.candH, req))
	if refRes != candRes {
		d.fail("response differs\nref:  %s\ncand: %s", refRes, candRes)
	}
}

// requestFor translates a generated op into the request a client would send.
// An append drawn with the "any" mode goes out without expected-version; the
// handler's rejection is then part of the compared behavior.
func requestFor(o op, cur int) protocol.Request {
	switch o.kind {
	case opArchive:
		return request(protocol.VerbArchive, o.path, nil, "")
	case opUnarchive:
		return request(protocol.VerbPublish, o.path, nil, "")
	}
	meta := maps.Clone(metaFor(o))
	if expected := expectedFor(o.expMode, cur); expected >= 0 {
		if meta == nil {
			meta = map[string]string{}
		}
		meta["expected-version"] = fmt.Sprint(expected)
	}
	verb := protocol.VerbPublish
	if o.kind == opAppend {
		verb = protocol.VerbAppend
	}
	return request(verb, o.path, meta, string(bodyFor(o)))
}

var rfc3339 = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`)

// normalize renders a response without wall-clock values: the modified key
// is dropped and timestamps in bodies are masked. LOOKUP rows are sorted
// because their modified tiebreak is wall clock too.
func normalize(resp protocol.Response) string {
	var sb strings.Builder
	sb.WriteString(resp.Status)
	for _, k := range slices.Sorted(maps.Keys(resp.Metadata)) {
		if k != "modified" {
			fmt.Fprintf(&sb, " %s=%q", k, resp.Metadata[k])
		}
	}
	body := rfc3339.ReplaceAllString(resp.Body, "<ts>")
	if strings.HasPrefix(body, "\n# Lookup matches") {
		lines := strings.Split(body, "\n")
		sort.Strings(lines)
		body = strings.Join(lines, "\n")
	}
	fmt.Fprintf(&sb, " body=%q", body)
	return sb.String()
}

func (d *handlerDifferential) compareSnapshots() {
	d.compareLines(handlerSnapshot(d.t, d.refH, d.ref), handlerSnapshot(d.t, d.candH, d.cand))
}

// handlerSnapshot sweeps every read verb over the pools through the handler.
func handlerSnapshot(t *testing.T, h *handler.Handler, b LookupBackend) []string {
	var lines []string
	add := func(label string, req protocol.Request) {
		lines = append(lines, label+" = "+normalize(Send(t, h, req)))
	}
	for _, p := range diffDocPaths {
		add("fetch "+p, request(protocol.VerbFetch, p, nil, ""))
		add("versions "+p, request(protocol.VerbVersions, p, nil, ""))
		cur := b.Store.CurrentVersion(p)
		for v := 1; v <= cur+1; v++ {
			add(fmt.Sprintf("fetch %s/v%d", p, v), request(protocol.VerbFetch, fmt.Sprintf("%s/v%d", p, v), nil, ""))
		}
	}
	for _, dir := range diffDirPaths {
		add("fetchdir "+dir, request(protocol.VerbFetch, dir, nil, ""))
		add("list "+dir, request(protocol.VerbList, dir, nil, ""))
		add("list-archived "+dir, request(protocol.VerbList, dir, map[string]string{"include-archived": "true"}, ""))
	}
	for i, body := range diffBodies {
		add(fmt.Sprintf("hash body=%d", i), request(protocol.VerbFetch, "/"+store.ContentHash(body), nil, ""))
	}
	for _, q := range diffQueries {
		for _, scope := range diffScopes {
			add(fmt.Sprintf("lookup q=%q scope=%q", q, scope), request(protocol.VerbLookup, scope, map[string]string{"query": q}, ""))
		}
	}
	for _, f := range diffFilters {
		add("lookup filter "+f, request(protocol.VerbLookup, "/", map[string]string{"query": "*", "filter": f}, ""))
	}
	return lines
}
