// demarkus-publish writes documents directly to a versioned content directory,
// bypassing the server. Use this when the server runs in read-only mode or inside
// a chroot — publish locally, serve publicly.
//
// Usage:
//
//	demarkus-publish -root /srv/site -path /index.md -body "# Hello"
//	demarkus-publish -root /srv/site -path /index.md < file.md
//	cat file.md | demarkus-publish -root /srv/site -path /index.md
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/store"
)

func main() {
	root := flag.String("root", "", "content directory (same as DEMARKUS_ROOT)")
	path := flag.String("path", "", "document path (e.g. /index.md)")
	body := flag.String("body", "", "document content (reads stdin if empty)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-publish [options]\n\n")
		fmt.Fprintf(os.Stderr, "Publish a document directly to the versioned store on disk.\n")
		fmt.Fprintf(os.Stderr, "Reads from stdin if -body is not provided.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *root == "" {
		*root = os.Getenv("DEMARKUS_ROOT")
	}
	if *root == "" {
		fmt.Fprintf(os.Stderr, "error: -root or DEMARKUS_ROOT is required\n")
		os.Exit(1)
	}
	if *path == "" {
		fmt.Fprintf(os.Stderr, "error: -path is required\n")
		os.Exit(1)
	}
	if !strings.HasPrefix(*path, "/") {
		fmt.Fprintf(os.Stderr, "error: -path must start with \"/\" (e.g. /index.md)\n")
		os.Exit(1)
	}
	if strings.HasPrefix(*path, "/sha256-") {
		fmt.Fprintf(os.Stderr, "error: /sha256-* paths are reserved for content-addressed fetch\n")
		os.Exit(1)
	}

	var content []byte
	if *body != "" {
		content = []byte(*body)
	} else {
		var err error
		content, err = io.ReadAll(io.LimitReader(os.Stdin, protocol.MaxBodyLength+1))
		if err != nil {
			fmt.Fprintf(os.Stderr, "error reading stdin: %v\n", err)
			os.Exit(1)
		}
	}

	if int64(len(content)) > protocol.MaxBodyLength {
		fmt.Fprintf(os.Stderr, "error: content exceeds %d byte limit\n", protocol.MaxBodyLength)
		os.Exit(1)
	}
	if len(content) == 0 {
		fmt.Fprintf(os.Stderr, "error: empty content\n")
		os.Exit(1)
	}

	s := store.New(*root)
	doc, err := s.Write(*path, content, nil)
	if err != nil {
		if errors.Is(err, store.ErrNotModified) {
			if doc != nil {
				fmt.Fprintf(os.Stderr, "unchanged (v%d)\n", doc.Version)
			} else {
				fmt.Fprintf(os.Stderr, "unchanged\n")
			}
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "published %s v%d\n", *path, doc.Version)
}
