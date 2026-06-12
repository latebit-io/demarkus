// demarkus-loadtest drives concurrent Mark Protocol requests at a server and
// reports throughput and latency percentiles. It exists to replace capacity
// guesswork with measured numbers on a given box.
//
// The single most important variable is connection reuse. QUIC's per-connection
// TLS 1.3 handshake costs single-digit milliseconds of server CPU, so a server
// that looks fast under reused connections can be an order of magnitude slower
// when every request opens a fresh connection. This tool measures both:
//
//	-conns mode (default): each worker holds one pooled QUIC connection and
//	  multiplexes its requests as streams — the warm, connection-reuse regime.
//	-fresh:                each request dials a new connection (full handshake)
//	  and closes it — the cold, no-reuse regime.
//
// Usage:
//
//	demarkus-loadtest -url mark://localhost:6309/index.md -c 64 -d 30s
//	demarkus-loadtest -url mark://localhost:6309/index.md -c 64 -d 30s -fresh
//	demarkus-loadtest -url mark://host/ -verb lookup -query architecture -c 16 -d 10s
//
// It only issues read verbs (FETCH, LIST, VERSIONS, LOOKUP) so a load run never
// mutates the store. Caching is disabled so every request hits the wire.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/protocol"
)

func main() {
	url := flag.String("url", "", "target mark:// URL (required)")
	concurrency := flag.Int("c", 16, "number of concurrent workers")
	duration := flag.Duration("d", 10*time.Second, "test duration (ignored if -n > 0)")
	count := flag.Int64("n", 0, "total requests to send (overrides -d when > 0)")
	fresh := flag.Bool("fresh", false, "dial a new connection per request (no-reuse regime)")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification (self-signed dev certs)")
	token := flag.String("token", "", "auth token for read-gated paths")
	verb := flag.String("verb", "fetch", "read verb: fetch | list | versions | lookup")
	query := flag.String("query", "", "subject for -verb lookup (required for lookup)")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-loadtest -url mark://host/path [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "error: -url is required")
		flag.Usage()
		os.Exit(2)
	}
	host, path, err := fetch.ParseMarkURL(*url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	op, err := resolveOp(*verb, *query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if *concurrency < 1 {
		fmt.Fprintln(os.Stderr, "error: -c must be >= 1")
		os.Exit(2)
	}

	// Ctrl-C ends the run early and still prints whatever was measured.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *count <= 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	clientOpts := fetch.Options{
		Insecure:       *insecure,
		RequestTimeout: *timeout,
		DialTimeout:    *timeout,
	}

	fmt.Printf("target:      %s%s\n", host, path)
	fmt.Printf("verb:        %s\n", *verb)
	fmt.Printf("concurrency: %d worker(s)\n", *concurrency)
	if *fresh {
		fmt.Printf("mode:        fresh connection per request (no reuse)\n")
	} else {
		fmt.Printf("mode:        %d reused connection(s)\n", *concurrency)
	}
	if *count > 0 {
		fmt.Printf("budget:      %d requests\n", *count)
	} else {
		fmt.Printf("budget:      %s\n", *duration)
	}
	fmt.Println("running...")

	var remaining atomic.Int64
	remaining.Store(*count)

	results := make([]workerResult, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range *concurrency {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = runWorker(ctx, host, path, *token, op, clientOpts, *fresh, *count > 0, &remaining)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	report(merge(results), elapsed)
}

// op performs one read request on a client and returns the response body size.
type op func(c *fetch.Client, host, path, token string) (fetch.Result, error)

func resolveOp(verb, query string) (op, error) {
	switch verb {
	case "fetch":
		return func(c *fetch.Client, h, p, t string) (fetch.Result, error) { return c.Fetch(h, p, t) }, nil
	case "list":
		return func(c *fetch.Client, h, p, t string) (fetch.Result, error) { return c.List(h, p, t) }, nil
	case "versions":
		return func(c *fetch.Client, h, p, t string) (fetch.Result, error) { return c.Versions(h, p, t) }, nil
	case "lookup":
		if query == "" {
			return nil, fmt.Errorf("-verb lookup requires -query")
		}
		return func(c *fetch.Client, h, p, t string) (fetch.Result, error) {
			return c.Lookup(h, p, query, t, fetch.LookupOptions{})
		}, nil
	default:
		return nil, fmt.Errorf("unknown -verb %q (want fetch|list|versions|lookup)", verb)
	}
}

type workerResult struct {
	latencies []time.Duration
	byStatus  map[string]int64
	errors    int64
	bodyBytes int64
	requests  int64
}

func runWorker(ctx context.Context, host, path, token string, do op, opts fetch.Options, fresh, byCount bool, remaining *atomic.Int64) workerResult {
	res := workerResult{byStatus: make(map[string]int64)}

	// In reuse mode the worker keeps one client (one pooled connection) for its
	// whole run, and a warmup request pays the handshake so it isn't charged to
	// the first measured request. In fresh mode the client is created per request.
	var client *fetch.Client
	if !fresh {
		client = fetch.NewClient(opts)
		defer client.Close()
		if _, err := do(client, host, path, token); err != nil {
			// A failed warmup is not fatal — the real requests will record it.
			_ = err
		}
	}

	for {
		if ctx.Err() != nil {
			return res
		}
		if byCount {
			if remaining.Add(-1) < 0 {
				return res
			}
		}

		c := client
		if fresh {
			c = fetch.NewClient(opts)
		}

		t0 := time.Now()
		result, err := do(c, host, path, token)
		lat := time.Since(t0)

		if fresh {
			c.Close()
		}

		res.requests++
		if err != nil {
			res.errors++
			continue
		}
		res.latencies = append(res.latencies, lat)
		res.byStatus[result.Response.Status]++
		res.bodyBytes += int64(len(result.Response.Body))
	}
}

func merge(parts []workerResult) workerResult {
	out := workerResult{byStatus: make(map[string]int64)}
	for _, p := range parts {
		out.latencies = append(out.latencies, p.latencies...)
		out.errors += p.errors
		out.bodyBytes += p.bodyBytes
		out.requests += p.requests
		for s, n := range p.byStatus {
			out.byStatus[s] += n
		}
	}
	return out
}

func report(r workerResult, elapsed time.Duration) {
	secs := elapsed.Seconds()
	ok := r.byStatus[protocol.StatusOK] + r.byStatus[protocol.StatusNotModified] + r.byStatus[protocol.StatusCreated]

	fmt.Println()
	fmt.Printf("elapsed:     %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("requests:    %d total, %d errored\n", r.requests, r.errors)
	fmt.Printf("throughput:  %.0f req/s\n", float64(r.requests)/secs)
	if r.bodyBytes > 0 {
		fmt.Printf("body data:   %.2f MB (%.2f MB/s)\n", float64(r.bodyBytes)/1e6, float64(r.bodyBytes)/1e6/secs)
	}

	if len(r.byStatus) > 0 {
		fmt.Print("status:      ")
		statuses := make([]string, 0, len(r.byStatus))
		for s := range r.byStatus {
			statuses = append(statuses, s)
		}
		sort.Strings(statuses)
		for i, s := range statuses {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s=%d", s, r.byStatus[s])
		}
		fmt.Println()
	}
	if ok < r.requests-r.errors {
		fmt.Println("note:        some responses were non-OK statuses — check the breakdown above")
	}

	lat := r.latencies
	if len(lat) == 0 {
		fmt.Println("latency:     no successful responses to measure")
		return
	}
	slices.Sort(lat)
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	fmt.Println("latency (successful responses):")
	fmt.Printf("  min  %s\n", lat[0].Round(time.Microsecond))
	fmt.Printf("  mean %s\n", (sum / time.Duration(len(lat))).Round(time.Microsecond))
	fmt.Printf("  p50  %s\n", percentile(lat, 0.50).Round(time.Microsecond))
	fmt.Printf("  p90  %s\n", percentile(lat, 0.90).Round(time.Microsecond))
	fmt.Printf("  p99  %s\n", percentile(lat, 0.99).Round(time.Microsecond))
	fmt.Printf("  max  %s\n", lat[len(lat)-1].Round(time.Microsecond))
}

// percentile returns the p-quantile (0..1) of a sorted slice using
// nearest-rank, which is stable and needs no interpolation.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(p * float64(len(sorted)))
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}
