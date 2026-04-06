// Command demarkus-agent is a federation crawler for the Mark Protocol.
// It discovers servers, collects content hashes, and publishes indexes to hubs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/latebit/demarkus/client/internal/fedcrawl"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/client/internal/tokens"
)

// version is set at build time via -ldflags.
var version = "dev"

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

func crawlMain(args []string) {
	fs := flag.NewFlagSet("crawl", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file")
	seeds := fs.String("seeds", "", "comma-separated seed servers (mark://host:port)")
	statePath := fs.String("state", fedcrawl.DefaultStatePath(), "path to state file")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	verbose := fs.Bool("v", false, "verbose output")
	publish := fs.Bool("publish", false, "publish indexes to hubs after crawl")
	perServer := fs.Bool("per-server", false, "publish per-server indexes instead of aggregated")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-agent crawl [options]\n\n")
		fmt.Fprintf(os.Stderr, "Runs a single federation crawl.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}

	// Parse seeds from flag or config.
	var seedList []string
	if *seeds != "" {
		seedList = strings.Split(*seeds, ",")
		for i, s := range seedList {
			seedList[i] = strings.TrimSpace(s)
		}
	}

	// Load config.
	cfg, err := fedcrawl.Load(*configPath, seedList)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Validate.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Create client.
	opts := fetch.Options{Insecure: *insecure}
	client := fetch.NewClient(opts)
	defer client.Close()

	// Load state.
	state, err := fedcrawl.LoadState(*statePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Load tokens.
	tokenStore := tokens.LoadDefault()

	// Run crawl.
	crawler := fedcrawl.NewCrawler(cfg, client, state, tokenStore)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *verbose {
		log.Printf("Starting crawl with %d seed(s)", len(cfg.Seeds))
	}

	start := time.Now()
	result, err := crawler.Run(ctx)
	if err != nil {
		log.Fatalf("crawl failed: %v", err)
	}

	elapsed := time.Since(start)

	if *verbose {
		log.Printf("Crawl complete in %s", elapsed.Round(time.Millisecond))
		log.Printf("  Servers discovered: %d", result.ServersDiscovered)
		log.Printf("  Documents crawled:  %d", result.DocumentsCrawled)
		log.Printf("  Hashes collected:   %d", result.HashesCollected)
		if len(result.Errors) > 0 {
			log.Printf("  Errors: %d", len(result.Errors))
			for _, e := range result.Errors {
				log.Printf("    - %s", e)
			}
		}
	} else {
		fmt.Printf("servers=%d docs=%d hashes=%d errors=%d\n",
			result.ServersDiscovered,
			result.DocumentsCrawled,
			result.HashesCollected,
			len(result.Errors))
	}

	// Publish indexes to hubs if requested
	if *publish && len(cfg.Hubs) > 0 {
		pubCount, err := crawler.PublishToHubs(ctx, client, *perServer)
		if err != nil {
			log.Printf("[WARN] publish to hubs: %v", err)
		} else if *verbose {
			log.Printf("  Published %d index(es) to %d hub(s)", pubCount, len(cfg.Hubs))
		}
	}
}

func daemonMain(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "", "path to TOML config file (required)")
	seeds := fs.String("seeds", "", "comma-separated seed servers (overrides config)")
	statePath := fs.String("state", fedcrawl.DefaultStatePath(), "path to state file")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	publish := fs.Bool("publish", false, "publish indexes to hubs after each crawl")
	perServer := fs.Bool("per-server", false, "publish per-server indexes instead of aggregated")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-agent daemon [options]\n\n")
		fmt.Fprintf(os.Stderr, "Runs federation crawl as a daemon on a schedule.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}

	// Validate required flags.
	if *configPath == "" {
		log.Fatal("config path is required; use -config flag")
	}

	// Parse seeds from flag.
	var seedList []string
	if *seeds != "" {
		seedList = strings.Split(*seeds, ",")
		for i, s := range seedList {
			seedList[i] = strings.TrimSpace(s)
		}
	}

	// Load config.
	cfg, err := fedcrawl.Load(*configPath, seedList)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Validate.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	// Create client.
	opts := fetch.Options{Insecure: *insecure}
	client := fetch.NewClient(opts)
	defer client.Close()

	// Load state.
	state, err := fedcrawl.LoadState(*statePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	// Load tokens.
	tokenStore := tokens.LoadDefault()

	log.Printf("demarkus-agent daemon starting (interval: %s)", cfg.Schedule.Interval)

	// Validate interval for daemon mode before starting.
	if cfg.Schedule.Interval <= 0 {
		log.Fatal("schedule.interval must be > 0 in daemon mode")
	}

	// Signal handling.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run initial crawl.
	runCrawl(ctx, &cfg, client, state, tokenStore, *publish, *perServer)

	// Schedule subsequent crawls.
	ticker := time.NewTicker(cfg.Schedule.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("daemon stopping")
			return
		case <-ticker.C:
			runCrawl(ctx, &cfg, client, state, tokenStore, *publish, *perServer)
		}
	}
}

func runCrawl(ctx context.Context, cfg *fedcrawl.Config, client *fetch.Client, state *fedcrawl.State, tokenStore *tokens.Store, publish, perServer bool) {
	crawler := fedcrawl.NewCrawler(*cfg, client, state, tokenStore)

	start := time.Now()
	result, err := crawler.Run(ctx)
	elapsed := time.Since(start)

	if err != nil {
		log.Printf("[ERROR] crawl failed: %v", err)
		return
	}

	log.Printf("[INFO] crawl complete in %s: servers=%d docs=%d hashes=%d errors=%d",
		elapsed.Round(time.Millisecond),
		result.ServersDiscovered,
		result.DocumentsCrawled,
		result.HashesCollected,
		len(result.Errors))

	for _, e := range result.Errors {
		log.Printf("[WARN] %s", e)
	}

	// Publish indexes to hubs if requested
	if publish && len(cfg.Hubs) > 0 {
		pubCount, err := crawler.PublishToHubs(ctx, client, perServer)
		if err != nil {
			log.Printf("[WARN] publish to hubs: %v", err)
		} else {
			log.Printf("[INFO] published %d index(es) to %d hub(s)", pubCount, len(cfg.Hubs))
		}
	}
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
