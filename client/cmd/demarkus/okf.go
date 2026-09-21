package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/okf"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/listwalk"
	"github.com/latebit-io/demarkus/protocol"
)

// okfMain dispatches `demarkus okf <subcommand>`.
func okfMain(ctx context.Context, args []string) {
	if len(args) == 0 {
		okfUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "validate":
		okfValidateMain(args[1:])
	case "import":
		okfImportMain(ctx, args[1:])
	case "export":
		okfExportMain(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "okf: unknown subcommand %q\n", args[0])
		okfUsage()
		os.Exit(2)
	}
}

func okfUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  demarkus okf validate [--strict] [--quiet] <bundle-dir>")
	fmt.Fprintln(os.Stderr, "  demarkus okf import [--dry-run] [--auth t] [--insecure] <bundle-dir> <mark://host/prefix>")
	fmt.Fprintln(os.Stderr, "  demarkus okf export [--auth t] [--insecure] <mark://host/prefix> <out-dir>")
}

// okfValidateMain checks an OKF bundle for v0.1 conformance, prints findings,
// and exits non-zero when the bundle does not conform.
func okfValidateMain(args []string) {
	fs := flag.NewFlagSet("okf validate", flag.ExitOnError)
	strict := fs.Bool("strict", false, "treat warnings as failures")
	quiet := fs.Bool("quiet", false, "print only the summary line")
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error
	if fs.NArg() != 1 {
		okfUsage()
		os.Exit(2)
	}

	report, err := okf.ValidateBundle(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf validate: %v\n", err)
		os.Exit(2)
	}

	if !*quiet {
		for _, f := range report.Findings {
			fmt.Printf("%s: %s: %s\n", f.Severity, f.Path, f.Message)
		}
	}
	errors, warnings := report.Counts()
	fmt.Printf("%d files, %d errors, %d warnings\n", report.Files, errors, warnings)

	if errors > 0 || (*strict && warnings > 0) {
		os.Exit(1)
	}
}

// okfImportMain mirrors an OKF bundle into a demarkus world: it parses each
// document, lifts the frontmatter into out-of-band metadata, rewrites
// bundle-absolute links under the target prefix, and publishes. Documents are
// upserted (no version check); re-importing an unchanged bundle creates no new
// versions thanks to the store's content-hash dedup.
func okfImportMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("okf import", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token for publishing (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	dryRun := fs.Bool("dry-run", false, "build and report the plan without publishing")
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error
	if fs.NArg() != 2 {
		okfUsage()
		os.Exit(2)
	}

	target, err := links.ParseMark(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf import: invalid URL: %v\n", err)
		os.Exit(2)
	}
	host, prefix := target.DialHost(), target.Path
	items, err := okf.BuildImport(fs.Arg(0), prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf import: %v\n", err)
		os.Exit(2)
	}

	for _, it := range items {
		for _, w := range it.Warnings {
			fmt.Printf("warning: %s: %s\n", it.Path, w)
		}
	}

	if *dryRun {
		fmt.Printf("%d documents planned (dry run, nothing published)\n", len(items))
		return
	}

	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, tokens.LoadDefault())
	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	published, failed := 0, 0
	for i, it := range items {
		// Interrupted: count the rest as failed instead of sending each to fail.
		if ctx.Err() != nil {
			fmt.Fprintf(os.Stderr, "error: interrupted, %d documents not published\n", len(items)-i)
			failed += len(items) - i
			break
		}
		// expectedVersion -1 skips the optimistic-concurrency check: import is
		// an upsert of the bundle as the source of truth.
		result, err := client.Publish(ctx, fetch.WriteRequest{
			Host: host, Path: it.Path, Token: token,
			Body: it.Body, ExpectedVersion: -1, Metadata: it.Metadata,
		})
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", it.Path, err)
			failed++
		case !protocol.IsWriteSuccess(result.Response.Status):
			fmt.Fprintf(os.Stderr, "error: %s: server returned %q\n", it.Path, result.Response.Status)
			failed++
		default:
			published++
			fmt.Printf("published %s (v%s)\n", it.Path, result.Response.Metadata["version"])
		}
	}
	fmt.Printf("%d published, %d failed\n", published, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// okfExportMain renders a world subtree into an OKF bundle under <out-dir>:
// LIST the subtree, fetch each document, reattach frontmatter from metadata,
// strip the world prefix from paths and links.
func okfExportMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("okf export", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token for reads on private paths (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	_ = fs.Parse(args) // ExitOnError: Parse exits, never returns an error
	if fs.NArg() != 2 {
		okfUsage()
		os.Exit(2)
	}

	target, err := links.ParseMark(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf export: invalid URL: %v\n", err)
		os.Exit(2)
	}
	host, prefix := target.DialHost(), target.Path
	outDir := fs.Arg(1)

	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, tokens.LoadDefault())
	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	root := prefix
	if root == "" {
		root = "/"
	}
	// No OnProblem: an export that silently lacks a subtree is worse than none.
	walker := listwalk.Walker{Client: client, Host: host, Token: token}
	paths, err := enumerateDocs(ctx, &walker, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
		os.Exit(1)
	}

	docs := make([]okf.ExportDoc, 0, len(paths))
	for _, p := range paths {
		result, err := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: p, Token: token})
		if err != nil {
			fmt.Fprintf(os.Stderr, "okf export: fetch %s: %v\n", p, err)
			os.Exit(1)
		}
		if result.Response.Status != protocol.StatusOK {
			fmt.Fprintf(os.Stderr, "okf export: fetch %s: server returned %q\n", p, result.Response.Status)
			os.Exit(1)
		}
		docs = append(docs, okf.ExportDoc{
			Path:     p,
			Body:     result.Response.Body,
			Metadata: publisherMeta(result.Response.Metadata),
			Modified: result.Response.Metadata["modified"],
		})
	}

	files, err := okf.BuildExport(docs, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
		os.Exit(1)
	}
	for _, f := range files {
		full := filepath.Join(outDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
			os.Exit(1)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s\n", f.Path)
	}
	fmt.Printf("%d documents exported to %s\n", len(docs), outDir)
}

func publisherMeta(respMeta map[string]string) map[string]string {
	out := make(map[string]string, len(respMeta))
	for k, v := range respMeta {
		if !protocol.IsReservedMetadataKey(k) {
			out[k] = v
		}
	}
	return out
}

// enumerateDocs recursively LISTs dir and returns the mark paths of every ".md"
// document beneath it, skipping the server's own version directories (already
// filtered server-side).
func enumerateDocs(ctx context.Context, w *listwalk.Walker, dir string) ([]string, error) {
	var docs []string
	err := w.Walk(ctx, dir, func(docPath string) error {
		if strings.HasSuffix(docPath, ".md") {
			docs = append(docs, docPath)
		}
		return nil
	})
	return docs, err
}
