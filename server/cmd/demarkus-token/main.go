package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/latebit/demarkus/server/internal/auth"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "generate" {
		fmt.Fprintf(os.Stderr, "usage: demarkus-token generate [-paths PATTERNS] [-ops OPERATIONS] [-tokens FILE]\n")
		os.Exit(1)
	}

	fs := flag.NewFlagSet("generate", flag.ExitOnError)
	paths := fs.String("paths", "/*", "comma-separated path patterns (e.g. \"/docs/*,/public/*\")")
	ops := fs.String("ops", "publish", "comma-separated operations (e.g. \"read,publish\")")
	tokensFile := fs.String("tokens", "", "path to tokens.toml file (appends entry if provided)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus-token generate [-paths PATTERNS] [-ops OPERATIONS] [-tokens FILE]\n\n")
		fmt.Fprintf(os.Stderr, "Generates a cryptographically random auth token for the Mark Protocol server.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parse flags: %v", err)
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
	entry := fmt.Sprintf("\n%q = { paths = [%s], operations = [%s] }\n",
		hashedToken,
		quotedList(pathList),
		quotedList(opsList),
	)

	if *tokensFile != "" {
		f, err := os.OpenFile(*tokensFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			log.Fatalf("open tokens file: %v", err)
		}
		// If the file is new/empty, write the [tokens] header.
		info, _ := f.Stat()
		if info.Size() == 0 {
			if _, err := f.WriteString("[tokens]\n"); err != nil {
				_ = f.Close()
				log.Fatalf("write tokens header: %v", err)
			}
		}
		if _, err := f.WriteString(entry); err != nil {
			_ = f.Close()
			log.Fatalf("write token entry: %v", err)
		}
		if err := f.Close(); err != nil {
			log.Fatalf("close tokens file: %v", err)
		}
		fmt.Fprintf(os.Stderr, "Token appended to %s\n", *tokensFile)
	} else {
		fmt.Fprintln(os.Stderr, "Add this to your tokens.toml under [tokens]:")
		fmt.Fprint(os.Stderr, entry)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Raw token (give to client, shown once):")
	fmt.Println(rawToken)
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
