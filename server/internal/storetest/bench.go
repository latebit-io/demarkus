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
		req  func(i int) protocol.Request
	}{
		{"publish-new", func(i int) protocol.Request {
			return request(protocol.VerbPublish, fmt.Sprintf("/new/n-%d.md", i), nil, "# new\n")
		}},
		{"publish-update", func(i int) protocol.Request {
			return request(protocol.VerbPublish, "/d00/doc-0000.md", nil, fmt.Sprintf("# v%d\n", i))
		}},
		{"fetch", func(int) protocol.Request { return request(protocol.VerbFetch, "/d01/doc-0001.md", nil, "") }},
		{"list-root", func(int) protocol.Request { return request(protocol.VerbList, "/", nil, "") }},
		{"list-dir", func(int) protocol.Request { return request(protocol.VerbList, "/d05", nil, "") }},
		{"lookup", func(int) protocol.Request {
			return request(protocol.VerbLookup, "/", map[string]string{"query": "t7 common"}, "")
		}},
		{"versions", func(int) protocol.Request { return request(protocol.VerbVersions, "/d02/doc-0002.md", nil, "") }},
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
				Send(b, h, c.req(i))
			}
		})
	}
}

// seedBenchHandler publishes the benchmark corpus: benchDocs documents over
// 40 directories and 50 tags, plus benchVersions versions of one document.
func seedBenchHandler(b *testing.B, factory func(testing.TB) LookupBackend) *handler.Handler {
	h := NewHandler(factory(b))
	for i := range benchDocs {
		p := fmt.Sprintf("/d%02d/doc-%04d.md", i%40, i)
		meta := map[string]string{"tags": fmt.Sprintf("t%d, common", i%50), "importance": "0.5"}
		Send(b, h, request(protocol.VerbPublish, p, meta, fmt.Sprintf("# Doc %d\n\nbody text here\n", i)))
	}
	for i := range benchVersions {
		Send(b, h, request(protocol.VerbPublish, "/d02/doc-0002.md", nil, fmt.Sprintf("# rev %d\n", i)))
	}
	return h
}
