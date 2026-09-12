package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/latebit-io/demarkus/tools/internal/answerbench"
	"github.com/latebit-io/demarkus/tools/internal/retrievalbench"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "corpus-pack", "corpus-restore":
			return runCorpus(ctx, os.Args[1], os.Args[2:])
		case "export-metrics":
			if len(os.Args) != 4 {
				return fmt.Errorf("usage: export-metrics PRIVATE.json NEW_METRICS.json")
			}
			return answerbench.ExportMetrics(os.Args[2], os.Args[3])
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "inspect" {
		return runInspect(ctx, os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "rescore" {
		return runRescore(ctx, os.Args[2:])
	}
	if len(os.Args) > 1 && (os.Args[1] == "compare" || os.Args[1] == "compare-policies") {
		return runCompare(os.Args[1], os.Args[2:])
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
		binary := flags.String("mcp-bin", "", "production MCP binary")
		host := flags.String("host", "", "fixture host:port")
		dial := flags.String("dial-address", "", "network route for the frozen origin")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		args := []string{"-host", "mark://" + *host, "-insecure", "-no-cache"}
		if *dial != "" {
			args = append(args, "-dial-address", *dial)
		}
		return answerbench.ServeProxy(ctx, retrievalbench.StdioConfig{Command: *binary, Args: args, Env: os.Environ()}, *host)
	}
	var cfg answerbench.Config
	flag.StringVar(&cfg.OpenCode, "opencode", "opencode", "OpenCode 1.18.30 binary")
	flag.StringVar(&cfg.Server, "server-bin", "server/bin/demarkus-server", "production server binary")
	flag.StringVar(&cfg.MCP, "mcp-bin", "client/bin/demarkus-mcp", "production MCP binary")
	flag.StringVar(&cfg.Output, "out", "", "new output directory (required)")
	flag.StringVar(&cfg.Temp, "temp", "", "temporary work parent; OS default when empty")
	flag.StringVar(&cfg.Model, "model", "openai/gpt-6-astra", "fixed provider/model")
	flag.StringVar(&cfg.Variant, "variant", "low", "fixed reasoning variant")
	flag.StringVar(&cfg.Case, "case", "", "one task ID for a smoke run; empty runs full suite")
	flag.StringVar(&cfg.Corpus, "corpus", "", "copied versioned store root; synthetic fixture when empty")
	flag.StringVar(&cfg.Questions, "questions", "", "directory with tasks.json and rubric.json for copied corpus")
	flag.StringVar(&cfg.Origin, "origin", "", "logical origin for copied corpus, e.g. mark://soul.demarkus.io")
	flag.StringVar(&cfg.ReaderPolicy, "reader-policy", "section-first", "reader policy: section-first or budget-body (original baseline)")
	flag.IntVar(&cfg.Port, "port", 16319, "unused local fixture port")
	flag.IntVar(&cfg.Repeats, "repeats", 2, "fresh sessions per question")
	flag.IntVar(&cfg.Steps, "steps", 8, "maximum reader iterations")
	flag.DurationVar(&cfg.Timeout, "timeout", 3*time.Minute, "per-question wall-clock bound")
	flag.Parse()
	if cfg.Output == "" {
		return fmt.Errorf("-out is required")
	}
	var err error
	for _, binary := range []*string{&cfg.OpenCode, &cfg.Server, &cfg.MCP} {
		*binary, err = exec.LookPath(*binary)
		if err != nil {
			return err
		}
		*binary, err = filepath.Abs(*binary)
		if err != nil {
			return err
		}
	}
	cfg.Proxy, err = os.Executable()
	if err != nil {
		return err
	}
	cfg.Log = os.Stderr
	report, runErr := answerbench.Run(ctx, &cfg)
	if err := json.NewEncoder(os.Stdout).Encode(report.Summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if runErr != nil {
		return runErr
	}
	if !report.Summary.UsageComplete {
		return fmt.Errorf("incomplete provider usage; tokens-per-correct is unavailable")
	}
	return nil
}

func runCompare(command string, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: demarkus-answer-bench %s BEFORE.json AFTER.json", command)
	}
	before, err := answerbench.LoadReport(args[0])
	if err != nil {
		return err
	}
	after, err := answerbench.LoadReport(args[1])
	if err != nil {
		return err
	}
	var comparison answerbench.Comparison
	if command == "compare-policies" {
		comparison, err = answerbench.ComparePolicies(&before, &after)
	} else {
		comparison, err = answerbench.Compare(&before, &after)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(comparison)
}

func runInspect(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	corpus := flags.String("corpus", "", "copied versioned corpus")
	questions := flags.String("questions", "", "tasks and rubric directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *corpus == "" || *questions == "" {
		return fmt.Errorf("inspect requires -corpus and -questions")
	}
	info, err := answerbench.InspectStore(ctx, *corpus, *questions)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(info)
}

func runRescore(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rescore", flag.ContinueOnError)
	corpus := flags.String("corpus", "", "copied corpus; embedded fixture when empty")
	questions := flags.String("questions", "", "copied corpus tasks and rubric")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 || (*corpus == "") != (*questions == "") {
		return fmt.Errorf("usage: rescore [-corpus ROOT -questions DIR] ORIGINAL.json NEW.json")
	}
	f, err := answerbench.LoadFixture()
	if *corpus != "" {
		f, err = answerbench.LoadStoreFixture(ctx, *corpus, *questions)
	}
	if err != nil {
		return err
	}
	report, err := answerbench.RescoreWithFixture(flags.Arg(0), flags.Arg(1), &f)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(report.Summary)
}
