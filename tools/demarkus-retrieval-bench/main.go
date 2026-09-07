// demarkus-retrieval-bench scores a question set through the MCP tools an
// agent uses and reports calls, result tokens, hit rate, and elapsed time.
// Each question gets a fresh demarkus-mcp process so session dedup cannot leak.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/retrievalbench"
)

func main() {
	host := flag.String("host", "", "demarkus endpoint, e.g. mark://soul.demarkus.io (required)")
	mcpBin := flag.String("mcp-bin", "demarkus-mcp", "demarkus-mcp binary to spawn per question")
	insecure := flag.Bool("insecure", false, "pass -insecure to demarkus-mcp")
	tokenFile := flag.String("token-file", "", "file holding the auth token; DEMARKUS_AUTH env is used when empty")
	questions := flag.String("questions", "", "question fixture path; embedded soul set when empty")
	scope := flag.String("scope", "", "lookup scope override, e.g. /demarkus/")
	strategy := flag.String("strategy", "lookup-fetch", "retrieval strategy: lookup-fetch")
	lookupLimit := flag.Int("limit", 10, "mark_lookup limit")
	maxFetches := flag.Int("max-fetches", 5, "fetch budget per question")
	timeout := flag.Duration("timeout", 60*time.Second, "per-question timeout")
	jsonOut := flag.String("json", "", "write the JSON report here")
	mdOut := flag.String("markdown", "", "write the markdown report here (also printed)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-retrieval-bench -host mark://host [options]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *host == "" {
		fmt.Fprintln(os.Stderr, "error: -host is required")
		flag.Usage()
		os.Exit(2)
	}

	set, err := loadQuestions(*questions, *scope)
	if err != nil {
		fail(err)
	}
	retrieval, err := retrievalbench.StrategyByName(*strategy, set.Scope, *lookupLimit, *maxFetches)
	if err != nil {
		fail(err)
	}
	counter, err := retrievalbench.NewO200kCounter()
	if err != nil {
		fail(err)
	}
	env, err := childEnv(*tokenFile)
	if err != nil {
		fail(err)
	}
	args := []string{"-host", *host, "-no-cache"}
	if *insecure {
		args = append(args, "-insecure")
	}
	cfg := retrievalbench.Config{
		Strategy: retrieval,
		Counter:  counter,
		Endpoint: *host,
		Log:      os.Stderr,
		Timeout:  *timeout,
		Open: func(ctx context.Context) (retrievalbench.Session, error) {
			return retrievalbench.OpenStdioSession(ctx, retrievalbench.StdioConfig{Command: *mcpBin, Args: args, Env: env})
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := retrievalbench.Run(ctx, &cfg, set)
	if err != nil {
		fail(err)
	}
	md := report.Markdown()
	fmt.Print(md)
	if *mdOut != "" {
		if err := os.WriteFile(*mdOut, []byte(md), 0o644); err != nil {
			fail(fmt.Errorf("write %s: %w", *mdOut, err))
		}
	}
	if *jsonOut != "" {
		if err := retrievalbench.WriteJSON(*jsonOut, &report); err != nil {
			fail(err)
		}
	}
	if failed := report.Failed(); failed > 0 {
		fail(fmt.Errorf("%d question(s) failed; see the error column", failed))
	}
}

func loadQuestions(path, scope string) (retrievalbench.QuestionSet, error) {
	var set retrievalbench.QuestionSet
	var err error
	if path == "" {
		set, err = retrievalbench.DefaultQuestionSet()
	} else {
		set, err = retrievalbench.LoadQuestionSet(path)
	}
	if err != nil {
		return set, err
	}
	if scope != "" {
		set.Scope = scope
		err = set.Validate()
	}
	return set, err
}

// childEnv passes the parent environment through and, with -token-file,
// overrides DEMARKUS_AUTH so demarkus-mcp authenticates like the plugin wrapper.
func childEnv(tokenFile string) ([]string, error) {
	env := os.Environ()
	if tokenFile == "" {
		return env, nil
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, fmt.Errorf("token file %s is empty", tokenFile)
	}
	kept := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "DEMARKUS_AUTH=") {
			kept = append(kept, kv)
		}
	}
	return append(kept, "DEMARKUS_AUTH="+token), nil
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	os.Exit(1)
}
