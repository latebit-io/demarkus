package storetest

import (
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/handler"
)

// benchDocs is the seeded corpus size; LIST / and LOOKUP scale with it.
// benchVersions is the history depth of the document VERSIONS is measured on.
const (
	benchDocs     = 2000
	benchVersions = 20
)

// RunHandlerBenchmarks measures each verb through the handler over a freshly
// seeded backend; compare backends with -bench Handler -benchtime 200x.
func RunHandlerBenchmarks(b *testing.B, factory func(testing.TB) LookupBackend) {
	cases := []struct {
		name string
		want string
		req  func(i int) protocol.Request
	}{
		{"publish-new", protocol.StatusCreated, func(i int) protocol.Request {
			return request(protocol.VerbPublish, fmt.Sprintf("/new/n-%d.md", i), nil, "# new\n")
		}},
		{"publish-update", protocol.StatusCreated, func(i int) protocol.Request {
			return request(protocol.VerbPublish, "/d00/doc-0000.md", nil, fmt.Sprintf("# v%d\n", i))
		}},
		{"fetch", protocol.StatusOK, func(int) protocol.Request { return request(protocol.VerbFetch, "/d01/doc-0001.md", nil, "") }},
		{"list-root", protocol.StatusOK, func(int) protocol.Request { return request(protocol.VerbList, "/", nil, "") }},
		{"list-dir", protocol.StatusOK, func(int) protocol.Request { return request(protocol.VerbList, "/d05", nil, "") }},
		{"lookup", protocol.StatusOK, func(int) protocol.Request {
			return request(protocol.VerbLookup, "/", map[string]string{"query": "t7 common"}, "")
		}},
		{"versions", protocol.StatusOK, func(int) protocol.Request { return request(protocol.VerbVersions, "/d02/doc-0002.md", nil, "") }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			// Each sub-benchmark starts from the same seeded state; b.N
			// calibration reruns must not see what an earlier one wrote.
			b.StopTimer()
			h := seedBenchHandler(b, factory)
			b.ResetTimer()
			b.StartTimer()
			for i := range b.N {
				if resp := Send(b, h, c.req(i)); resp.Status != c.want {
					b.Fatalf("%s: status = %q, want %q (%s)", c.name, resp.Status, c.want, resp.Body)
				}
			}
		})
	}
}

// seedBenchHandler publishes the benchmark corpus: benchDocs documents over
// 40 directories and 50 tags, one of them at exactly benchVersions versions.
func seedBenchHandler(b *testing.B, factory func(testing.TB) LookupBackend) *handler.Handler {
	h := NewHandler(factory(b))
	publish := func(req protocol.Request) {
		if resp := Send(b, h, req); resp.Status != protocol.StatusCreated {
			b.Fatalf("seed %s: status = %q (%s)", req.Path, resp.Status, resp.Body)
		}
	}
	for i := range benchDocs {
		p := fmt.Sprintf("/d%02d/doc-%04d.md", i%40, i)
		meta := map[string]string{"tags": fmt.Sprintf("t%d, common", i%50), "importance": "0.5"}
		publish(request(protocol.VerbPublish, p, meta, fmt.Sprintf("# Doc %d\n\nbody text here\n", i)))
	}
	for i := 1; i < benchVersions; i++ { // the corpus publish is version 1
		publish(request(protocol.VerbPublish, "/d02/doc-0002.md", nil, fmt.Sprintf("# rev %d\n", i)))
	}
	return h
}
