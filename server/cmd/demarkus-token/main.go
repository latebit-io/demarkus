package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/latebit/demarkus/server/internal/auth"
)

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

	// Generate 32 random bytes → 64 hex chars.
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		log.Fatalf("generate random bytes: %v", err)
	}
	rawToken := hex.EncodeToString(secret)
	hashedToken := auth.HashToken(rawToken)

	pathList := splitTrimmed(*paths)
	opsList := splitTrimmed(*ops)

	// Format TOML entry.
	entry := fmt.Sprintf("\n[tokens.%s]\nhash = %q\npaths = [%s]\noperations = [%s]\n",
		*label,
		hashedToken,
		quotedList(pathList),
		quotedList(opsList),
	)

	if *tokensFile != "" {
		f, err := os.OpenFile(*tokensFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("open tokens file: %v", err)
		}
		if _, err := f.WriteString(entry); err != nil {
			_ = f.Close()
			log.Fatalf("write token entry: %v", err)
		}
		if err := f.Close(); err != nil {
			log.Fatalf("close tokens file: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token %q appended to %s\n", *label, *tokensFile)
	} else {
		fmt.Fprintln(os.Stderr, "Add this to your tokens.toml:")
		fmt.Fprint(os.Stderr, entry)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Raw token (give to client, shown once):")
	fmt.Println(rawToken)
}

// tokensFileData is the TOML structure for reading the full file.
type tokensFileData struct {
	Tokens map[string]tokenEntry `toml:"tokens"`
}

type tokenEntry struct {
	Hash       string   `toml:"hash"`
	Paths      []string `toml:"paths"`
	Operations []string `toml:"operations"`
	Expires    string   `toml:"expires,omitempty"`
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

	var tf tokensFileData
	if _, err := toml.DecodeFile(*tokensFile, &tf); err != nil {
		log.Fatalf("read tokens file: %v", err)
	}

	if len(tf.Tokens) == 0 {
		fmt.Println("No tokens found.")
		return
	}

	for label, tok := range tf.Tokens {
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

	var tf tokensFileData
	if _, err := toml.DecodeFile(*tokensFile, &tf); err != nil {
		log.Fatalf("read tokens file: %v", err)
	}

	if _, ok := tf.Tokens[*label]; !ok {
		log.Fatalf("token %q not found in %s", *label, *tokensFile)
	}

	delete(tf.Tokens, *label)

	f, err := os.Create(*tokensFile)
	if err != nil {
		log.Fatalf("open tokens file for writing: %v", err)
	}
	if err := toml.NewEncoder(f).Encode(tf); err != nil {
		_ = f.Close()
		log.Fatalf("write tokens file: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Fatalf("close tokens file: %v", err)
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

func quotedList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return strings.Join(quoted, ", ")
}
