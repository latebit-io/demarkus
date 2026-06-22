package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/okf"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// okfMain dispatches `demarkus okf <subcommand>`.
func okfMain(args []string) {
	if len(args) == 0 {
		okfUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "validate":
		okfValidateMain(args[1:])
	case "import":
		okfImportMain(args[1:])
	case "export":
		okfExportMain(args[1:])
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
	_ = fs.Parse(args)
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
func okfImportMain(args []string) {
	fs := flag.NewFlagSet("okf import", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token for publishing (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	dryRun := fs.Bool("dry-run", false, "build and report the plan without publishing")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		okfUsage()
		os.Exit(2)
	}

	host, prefix, err := fetch.ParseMarkURL(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf import: %v\n", err)
		os.Exit(2)
	}
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

	token := tokens.Resolve(*authToken, host, tokens.LoadDefault())
	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	published, failed := 0, 0
	for _, it := range items {
		// expectedVersion -1 skips the optimistic-concurrency check: import is
		// an upsert of the bundle as the source of truth.
		result, err := client.Publish(host, it.Path, it.Body, token, -1, it.Metadata)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", it.Path, err)
			failed++
		case result.Response.Status != protocol.StatusOK && result.Response.Status != protocol.StatusCreated:
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

// serverRespKeys are response metadata fields owned by the server; everything
// else in a FETCH response is publisher metadata to carry into the bundle.
var serverRespKeys = map[string]bool{
	"status": true, "version": true, "modified": true, "etag": true,
	"content-hash": true, "current-version": true, "entries": true,
}

// okfExportMain renders a demarkus world subtree into an OKF bundle on disk:
// it enumerates the subtree with LIST, fetches each document, reattaches OKF
// frontmatter from its metadata, strips the world prefix from paths and links,
// and writes the tree under <out-dir>.
func okfExportMain(args []string) {
	fs := flag.NewFlagSet("okf export", flag.ExitOnError)
	authToken := fs.String("auth", "", "auth token for reads on private paths (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	_ = fs.Parse(args)
	if fs.NArg() != 2 {
		okfUsage()
		os.Exit(2)
	}

	host, prefix, err := fetch.ParseMarkURL(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
		os.Exit(2)
	}
	outDir := fs.Arg(1)

	token := tokens.Resolve(*authToken, host, tokens.LoadDefault())
	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	root := prefix
	if root == "" {
		root = "/"
	}
	paths, err := enumerateDocs(client, host, root, token, map[string]struct{}{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "okf export: %v\n", err)
		os.Exit(1)
	}

	docs := make([]okf.ExportDoc, 0, len(paths))
	for _, p := range paths {
		result, err := client.Fetch(host, p, token)
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
		if !serverRespKeys[k] {
			out[k] = v
		}
	}
	return out
}

// enumerateDocs recursively LISTs dir and returns the mark paths of every ".md"
// document beneath it, skipping the server's own version directories (already
// filtered server-side).
func enumerateDocs(client *fetch.Client, host, dir, token string, seen map[string]struct{}) ([]string, error) {
	if _, ok := seen[dir]; ok {
		return nil, fmt.Errorf("list cycle detected at %s", dir)
	}
	seen[dir] = struct{}{}

	result, err := client.List(host, dir, token)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	if result.Response.Status != protocol.StatusOK {
		return nil, fmt.Errorf("list %s: server returned %q", dir, result.Response.Status)
	}
	var docs []string
	for _, dest := range links.Extract(result.Response.Body) {
		name, err := url.PathUnescape(dest)
		if err != nil {
			return nil, fmt.Errorf("list %s: decode entry %q: %w", dir, dest, err)
		}
		if sub, ok := strings.CutSuffix(name, "/"); ok {
			child, err := enumerateDocs(client, host, path.Join(dir, sub), token, seen)
			if err != nil {
				return nil, err
			}
			docs = append(docs, child...)
		} else if strings.HasSuffix(name, ".md") {
			docs = append(docs, path.Join(dir, name))
		}
	}
	return docs, nil
}
