// demarkus-token mints, lists, and revokes capability tokens for a Mark
// Protocol server. The TOML file it writes is consumed by the server's
// auth.LoadTokens at startup (and on SIGHUP).
//
// Usage:
//
//	demarkus-token generate -label fritz-laptop -paths "/docs/*" -ops "publish" -tokens tokens.toml
//	demarkus-token list -tokens tokens.toml
//	demarkus-token revoke -label fritz-laptop -tokens tokens.toml
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/latebit-io/demarkus/protocol/token"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "generate":
		cmdGenerate(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "revoke":
		cmdRevoke(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "usage: demarkus-token <command> [flags]\n\n")
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  generate  Generate a new auth token\n")
	fmt.Fprintf(os.Stderr, "  list      List tokens in a tokens file\n")
	fmt.Fprintf(os.Stderr, "  revoke    Revoke a token by label\n")
	fmt.Fprintf(os.Stderr, "  version   Print version and exit\n")
}

func cmdGenerate(args []string) {
	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	label := fs.String("label", "", "human-readable label for this token (required)")
	paths := fs.String("paths", "/*", "comma-separated path patterns (e.g. \"/docs/*,/public/*\")")
	ops := fs.String("ops", "publish", "comma-separated operations (e.g. \"read,publish\")")
	tokensFile := fs.String("tokens", "", "path to tokens.toml file (appends entry if provided)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-token generate -label NAME [-paths PATTERNS] [-ops OPERATIONS] [-tokens FILE]\n\n")
		fmt.Fprintf(os.Stderr, "Generates a cryptographically random auth token for the Mark Protocol server.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}

	if *label == "" {
		fmt.Fprintf(os.Stderr, "error: -label is required\n\n")
		fs.Usage()
		os.Exit(1)
	}

	minted, err := token.Generate(*label, splitTrimmed(*paths), splitTrimmed(*ops))
	if err != nil {
		log.Fatalf("generate: %v", err)
	}

	if *tokensFile != "" {
		if err := token.AppendEntry(*tokensFile, minted.Label, &minted.Entry); err != nil {
			if errors.Is(err, token.ErrLabelExists) {
				log.Fatalf("label %q already exists in %s", minted.Label, *tokensFile)
			}
			log.Fatalf("%v", err)
		}
		fmt.Fprintf(os.Stderr, "Token %q appended to %s\n", minted.Label, *tokensFile)
	} else {
		fmt.Fprintln(os.Stderr, "Add this to your tokens.toml:")
		fmt.Fprint(os.Stderr, token.FormatEntry(minted.Label, &minted.Entry))
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Raw token (give to client, shown once):")
	fmt.Println(minted.Raw)
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	tokensFile := fs.String("tokens", "", "path to tokens.toml file (required)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-token list -tokens FILE\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if *tokensFile == "" {
		fmt.Fprintf(os.Stderr, "error: -tokens is required\n\n")
		fs.Usage()
		os.Exit(1)
	}

	file, err := token.ReadFile(*tokensFile)
	if err != nil {
		log.Fatalf("%v", err)
	}

	if len(file.Tokens) == 0 {
		fmt.Println("No tokens found.")
		return
	}

	labels := make([]string, 0, len(file.Tokens))
	for label := range file.Tokens {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		tok := file.Tokens[label]
		paths := strings.Join(tok.Paths, ", ")
		ops := strings.Join(tok.Operations, ", ")
		fmt.Printf("%-20s  paths: %-20s  ops: %s\n", label, paths, ops)
	}
}

func cmdRevoke(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ExitOnError)
	label := fs.String("label", "", "label of the token to revoke (required)")
	tokensFile := fs.String("tokens", "", "path to tokens.toml file (required)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-token revoke -label NAME -tokens FILE\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if *label == "" || *tokensFile == "" {
		fmt.Fprintf(os.Stderr, "error: -label and -tokens are required\n\n")
		fs.Usage()
		os.Exit(1)
	}

	file, err := token.ReadFile(*tokensFile)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if _, ok := file.Tokens[*label]; !ok {
		log.Fatalf("token %q not found in %s", *label, *tokensFile)
	}
	delete(file.Tokens, *label)

	if err := token.WriteFile(*tokensFile, file); err != nil {
		log.Fatalf("%v", err)
	}

	fmt.Fprintf(os.Stderr, "Revoked token %q\n", *label)
}

func splitTrimmed(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
