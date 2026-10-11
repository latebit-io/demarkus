// Command demarkus-agent is a federation crawler for the Mark Protocol.
// It discovers servers, accumulates the link graph, and publishes it to hubs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/fedcrawl"
	"github.com/latebit-io/demarkus/client/internal/tokens"
)

// version is set at build time via -ldflags.
var version = "dev"

// logger is the package-level structured logger. Format is text by default
// and switches to JSON when DEMARKUS_LOG_FORMAT=json — matches the server
// + broker convention so an operator's log shipper sees the same shape
// across all three runtime images.
var logger = newLogger()

func newLogger() *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	// Validate explicitly so a typo (DEMARKUS_LOG_FORMAT=jsno) fails fast
	// at startup rather than silently producing text logs an operator's
	// JSON-aware log shipper can't parse.
	format := strings.ToLower(strings.TrimSpace(os.Getenv("DEMARKUS_LOG_FORMAT")))
	switch format {
	case "", "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	default:
		fmt.Fprintf(os.Stderr, "invalid DEMARKUS_LOG_FORMAT=%q (expected: json|text)\n", format)
		os.Exit(2)
		return nil
	}
}

// fatal logs at ERROR level with the provided fields and exits non-zero.
// Replacement for the previous log.Fatalf / log.Fatal pattern.
func fatal(msg string, args ...any) {
	logger.Error(msg, args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "crawl":
			crawlMain(os.Args[2:])
			return
		case "daemon":
			daemonMain(os.Args[2:])
			return
		case "version":
			fmt.Println("demarkus-agent", version)
			return
		}
	}
	flag.Usage()
	os.Exit(1)
}

// crawlFlags are the flags crawl and daemon share.
type crawlFlags struct {
	configPath, seeds, statePath string
	insecure, publishGraph       bool
	publishRetention             int
}

func newCrawlFlagSet(name string, handling flag.ErrorHandling) (*flag.FlagSet, *crawlFlags) {
	fs := flag.NewFlagSet(name, handling)
	f := &crawlFlags{}
	fs.StringVar(&f.configPath, "config", "", "path to TOML config file")
	fs.StringVar(&f.seeds, "seeds", "", "comma-separated seed servers (mark://host:port)")
	fs.StringVar(&f.statePath, "state", fedcrawl.DefaultStatePath(), "path to state file")
	fs.BoolVar(&f.insecure, "insecure", false, "skip TLS certificate verification")
	fs.BoolVar(&f.publishGraph, "publish-graph", false, "publish a link-graph export (/graph.md) to hubs after each crawl")
	fs.IntVar(&f.publishRetention, "publish-retention", -1, "cap the /graph.md and /graph/manifest.md histories to their newest N versions; version-pinned shards stay unpruned; 0 keeps every version; -1 uses the config value (default 20)")
	return fs, f
}

func crawlMain(args []string) {
	fs, f := newCrawlFlagSet("crawl", flag.ExitOnError)
	verbose := fs.Bool("v", false, "verbose output")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-agent crawl [options]\n\n")
		fmt.Fprintf(os.Stderr, "Runs a single federation crawl.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		fatal("parse flags", "err", err)
	}

	// Parse seeds from flag or config.
	var seedList []string
	if f.seeds != "" {
		seedList = strings.Split(f.seeds, ",")
		for i, s := range seedList {
			seedList[i] = strings.TrimSpace(s)
		}
	}

	// Load config.
	cfg, err := fedcrawl.Load(f.configPath, seedList)
	if err != nil {
		fatal("load config", "err", err)
	}

	// CLI retention overrides config when set (-1 keeps the config value).
	// Anything else negative is a mistake; fail loudly instead of silently
	// falling back to the config value.
	if f.publishRetention < -1 {
		fatal("invalid -publish-retention (want -1, 0, or a positive count)", "value", f.publishRetention)
	}
	if f.publishRetention >= 0 {
		cfg.Publish.Retention = f.publishRetention
	}

	// Validate.
	if err := cfg.Validate(); err != nil {
		fatal("invalid config", "err", err)
	}

	// Create client.
	opts := fetch.Options{Insecure: f.insecure, Endpoints: cfg.ClientEndpoints()}
	client := fetch.NewClient(opts)
	defer client.Close()

	// Load state.
	state, err := fedcrawl.LoadState(f.statePath)
	if err != nil {
		fatal("load state", "err", err)
	}

	// Load tokens.
	tokenStore := tokens.LoadDefault()

	// Run crawl.
	crawler := fedcrawl.NewCrawler(cfg, client, state, tokenStore)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *verbose {
		logger.Info("crawl: starting", "seeds", len(cfg.Seeds))
	}

	start := time.Now()
	result, err := crawler.Run(ctx)
	if err != nil {
		fatal("crawl: failed", "err", err)
	}

	elapsed := time.Since(start)

	if *verbose {
		logger.Info("crawl: complete",
			"duration_ms", elapsed.Round(time.Millisecond).Milliseconds(),
			"servers", result.ServersDiscovered,
			"documents", result.DocumentsCrawled,
			"errors", len(result.Errors))
		for _, e := range result.Errors {
			logger.Warn("crawl: error", "detail", e)
		}
	} else {
		// Single-line stdout summary preserved for shell-script parsing.
		// Not routed through slog because it's user-facing output, not a log event.
		fmt.Printf("servers=%d docs=%d errors=%d\n",
			result.ServersDiscovered,
			result.DocumentsCrawled,
			len(result.Errors))
	}
	if blockIncompletePublication(result, f.publishGraph, len(cfg.Hubs)) {
		fatal("publish: skipped because crawl is incomplete", "errors", len(result.Errors))
	}

	// Publish the link-graph export to hubs if requested.
	if f.publishGraph && len(cfg.Hubs) > 0 {
		gCount, err := crawler.PublishGraphToHubs(ctx, client)
		if err != nil {
			logger.Warn("publish-graph: hub push failed", "err", err)
		} else if *verbose {
			logger.Info("publish-graph: complete", "graphs", gCount, "hubs", len(cfg.Hubs))
		}
	}
}

func daemonMain(args []string) {
	fs, f := newCrawlFlagSet("daemon", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-agent daemon [options]\n\n")
		fmt.Fprintf(os.Stderr, "Runs federation crawl as a daemon on a schedule.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		fatal("parse flags", "err", err)
	}

	// Validate required flags.
	if f.configPath == "" {
		fatal("config path is required; use -config flag")
	}

	// Parse seeds from flag.
	var seedList []string
	if f.seeds != "" {
		seedList = strings.Split(f.seeds, ",")
		for i, s := range seedList {
			seedList[i] = strings.TrimSpace(s)
		}
	}

	// Load config.
	cfg, err := fedcrawl.Load(f.configPath, seedList)
	if err != nil {
		fatal("load config", "err", err)
	}

	// CLI retention overrides config when set (-1 keeps the config value).
	// Anything else negative is a mistake; fail loudly instead of silently
	// falling back to the config value.
	if f.publishRetention < -1 {
		fatal("invalid -publish-retention (want -1, 0, or a positive count)", "value", f.publishRetention)
	}
	if f.publishRetention >= 0 {
		cfg.Publish.Retention = f.publishRetention
	}

	// Validate.
	if err := cfg.Validate(); err != nil {
		fatal("invalid config", "err", err)
	}

	// Create client.
	opts := fetch.Options{Insecure: f.insecure, Endpoints: cfg.ClientEndpoints()}
	client := fetch.NewClient(opts)
	defer client.Close()

	// Load state.
	state, err := fedcrawl.LoadState(f.statePath)
	if err != nil {
		fatal("load state", "err", err)
	}

	// Load tokens.
	tokenStore := tokens.LoadDefault()

	// Validate interval for daemon mode before starting.
	if cfg.Schedule.Interval <= 0 {
		fatal("schedule.interval must be > 0 in daemon mode")
	}

	logger.Info("daemon: starting", "interval", cfg.Schedule.Interval.String())

	// Signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run initial crawl.
	runCrawl(ctx, &cfg, client, state, tokenStore, f.publishGraph)

	// Schedule subsequent crawls.
	ticker := time.NewTicker(cfg.Schedule.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("daemon: stopping")
			return
		case <-ticker.C:
			runCrawl(ctx, &cfg, client, state, tokenStore, f.publishGraph)
		}
	}
}

func runCrawl(ctx context.Context, cfg *fedcrawl.Config, client *fetch.Client, state *fedcrawl.State, tokenStore *tokens.Store, publishGraph bool) {
	crawler := fedcrawl.NewCrawler(*cfg, client, state, tokenStore)

	start := time.Now()
	result, err := crawler.Run(ctx)
	elapsed := time.Since(start)

	if err != nil {
		logger.Error("crawl: failed", "err", err)
		return
	}

	logger.Info("crawl: complete",
		"duration_ms", elapsed.Round(time.Millisecond).Milliseconds(),
		"servers", result.ServersDiscovered,
		"documents", result.DocumentsCrawled,
		"errors", len(result.Errors))

	for _, e := range result.Errors {
		logger.Warn("crawl: error", "detail", e)
	}
	if blockIncompletePublication(result, publishGraph, len(cfg.Hubs)) {
		logger.Error("publish: skipped because crawl is incomplete", "errors", len(result.Errors))
		return
	}

	// Publish the link-graph export to hubs if requested.
	if publishGraph && len(cfg.Hubs) > 0 {
		gCount, err := crawler.PublishGraphToHubs(ctx, client)
		if err != nil {
			logger.Warn("publish-graph: hub push failed", "err", err)
		} else {
			logger.Info("publish-graph: complete", "graphs", gCount, "hubs", len(cfg.Hubs))
		}
	}
}

func blockIncompletePublication(result *fedcrawl.CrawlResult, publishGraph bool, hubCount int) bool {
	return result.Incomplete && hubCount > 0 && publishGraph
}

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-agent <command> [options]\n\n")
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  crawl   Run a single federation crawl\n")
		fmt.Fprintf(os.Stderr, "  daemon  Run federation crawl on a schedule\n")
		fmt.Fprintf(os.Stderr, "  version Print version\n")
	}
}
