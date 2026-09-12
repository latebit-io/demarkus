package broker

import (
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

// Quality metrics observe defects without making a pre-fix baseline un-runnable.
// Each fix must also add an enforcing regression test.
func BenchmarkGraphBaselineTenantSources(b *testing.B) {
	for _, tool := range []string{"mark_backlinks", "mark_explore"} {
		b.Run(tool, func(b *testing.B) {
			d := seededDispatcher()
			d.published["alice-w/private.md"] = fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   "# ALICE_PRIVATE_TITLE\n[reference](mark://bob-w/index.md)\n",
			}}
			d.published["bob-w/own.md"] = fetch.Result{Response: protocol.Response{
				Status: protocol.StatusOK,
				Body:   "# BOB_ALLOWED_TITLE\n[reference](mark://bob-w/index.md)\n",
			}}
			g := newMemoryGateway(b, memoryTestConfig(), d)
			bob := ctxWithClaims(b.Context(), &Claims{Subject: "google|bob", Email: "bob@example.com", EmailVerified: true})
			crawl := g.tenantGate(g.toolHandlers()["mark_graph"])
			res, err := crawl(withAliceClaims(b.Context()), callToolReq("mark_graph", map[string]any{"url": "mark://alice-w/private.md"}))
			if err != nil || res == nil || res.IsError {
				b.Fatalf("Alice crawl: %v, result=%v", err, res)
			}
			res, err = crawl(bob, callToolReq("mark_graph", map[string]any{"url": "mark://bob-w/own.md"}))
			if err != nil || res == nil || res.IsError {
				b.Fatalf("Bob crawl: %v, result=%v", err, res)
			}
			handler := g.tenantGate(g.toolHandlers()[tool])
			req := callToolReq(tool, map[string]any{"url": "mark://bob-w/index.md"})
			var leaks, missing int
			for b.Loop() {
				res, err := handler(bob, req)
				if err != nil || res == nil || res.IsError {
					b.Fatalf("%s: %v, result=%v", tool, err, res)
				}
				text := toolResultText(b, res)
				if strings.Contains(text, "ALICE_PRIVATE_TITLE") || strings.Contains(text, "alice-w/private.md") {
					leaks++
				}
				if !strings.Contains(text, "bob-w/own.md") {
					missing++
				}
				// Fake dispatchers retain a call log; don't let iterations grow it.
				d.mu.Lock()
				d.fetchCalls, d.fetchCondCalls, d.listCalls = nil, nil, nil
				d.mu.Unlock()
			}
			b.ReportMetric(float64(leaks)/float64(b.N), "leaked-responses/op")
			b.ReportMetric(float64(missing)/float64(b.N), "missing-own-source/op")
		})
	}
}
