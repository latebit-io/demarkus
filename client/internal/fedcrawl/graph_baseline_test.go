package fedcrawl

import (
	"testing"

	"github.com/latebit-io/demarkus/client/links"
)

func BenchmarkGraphBaselineRelativeTargets(b *testing.B) {
	for _, tc := range []struct {
		name, path, destination, want string
	}{
		{"root", "/source.md", "sibling.md", "mark://example.com/sibling.md"},
		{"nested", "/docs/source.md", "sibling.md", "mark://example.com/docs/sibling.md"},
		{"parent", "/docs/deep/source.md", "../sibling.md", "mark://example.com/docs/sibling.md"},
		{"absolute", "/docs/source.md", "/sibling.md", "mark://example.com/sibling.md"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := "# Source\n[reference](" + tc.destination + ")\n"
			var wrong int
			for b.Loop() {
				crawler := NewCrawler(DefaultConfig(), nil, nil, nil)
				crawler.recordEdges("example.com:6309", tc.path, body, nil)
				edges := crawler.graph.GetEdges()
				if len(edges) != 1 || links.CanonicalURL(edges[0].To) != tc.want {
					wrong++
				}
			}
			b.ReportMetric(float64(wrong)/float64(b.N), "wrong-targets/op")
		})
	}
}
