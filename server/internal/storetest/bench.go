package storetest

import (
	"fmt"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
)

// benchDocs is the seeded corpus size; LIST / and LOOKUP scale with it.
const benchDocs = 2000

// RunHandlerBenchmarks measures each verb through the handler over a backend
// seeded with benchDocs documents. Run against every backend and compare:
//
//	go test ./internal/storetest/ ./internal/pgstore/ -run '^$' -bench Handler -benchtime 200x
func RunHandlerBenchmarks(b *testing.B, factory func(testing.TB) LookupBackend) {
	h := NewHandler(factory(b))
	for i := range benchDocs {
		p := fmt.Sprintf("/d%02d/doc-%04d.md", i%40, i)
		meta := map[string]string{"tags": fmt.Sprintf("t%d, common", i%50), "importance": "0.5"}
		Send(b, h, request(protocol.VerbPublish, p, meta, fmt.Sprintf("# Doc %d\n\nbody text here\n", i)))
	}
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
		{"versions", func(int) protocol.Request { return request(protocol.VerbVersions, "/d00/doc-0000.md", nil, "") }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			for i := range b.N {
				Send(b, h, c.req(i))
			}
		})
	}
}
