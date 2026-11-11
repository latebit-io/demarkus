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
	pathpkg "path"
	"strings"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/store"
)

// exitNotSynced is the status for a publish that is current but whose
// directory sync, or notice to a running server, failed.
const exitNotSynced = 2

func main() {
	root := flag.String("root", "", "content directory (same as DEMARKUS_ROOT)")
	path := flag.String("path", "", "document path (e.g. /index.md)")
	body := flag.String("body", "", "document content (reads stdin if empty)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: demarkus-publish [options]\n\n")
		fmt.Fprintf(os.Stderr, "Publish a document directly to the versioned store on disk.\n")
		fmt.Fprintf(os.Stderr, "Reads from stdin if -body is not provided.\n\n")
		fmt.Fprintf(os.Stderr, "Exit status: 0 published or unchanged, 1 failed, %d published but the\n", exitNotSynced)
		fmt.Fprintf(os.Stderr, "directory sync or the notice to a running server failed (the version is current).\n\n")
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
	for _, r := range *path {
		if r == 0 || (r < 32 && r != '\t') || r == 127 {
			fmt.Fprintf(os.Stderr, "error: -path contains control characters\n")
			os.Exit(1)
		}
	}
	*path = pathpkg.Clean(*path)
	if !strings.HasPrefix(*path, "/") {
		fmt.Fprintf(os.Stderr, "error: -path must start with \"/\" (e.g. /index.md)\n")
		os.Exit(1)
	}
	if *path == "/" {
		fmt.Fprintf(os.Stderr, "error: -path must be a document path, not \"/\"\n")
		os.Exit(1)
	}
	if protocol.IsReservedHashPath(*path) {
		fmt.Fprintf(os.Stderr, "error: /sha256-<hash> paths are reserved\n")
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
	if storefmt.Committed(err) {
		// A server holding the root open indexes again on its next read.
		if markErr := s.MarkChanged(); markErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: a running server serves the old index until restarted: %w", storefmt.ErrCommittedNotSynced, markErr))
		}
	}
	if errors.Is(err, storefmt.ErrCommittedNotSynced) {
		// The version is current; only its directory sync or the server
		// notice failed, which the exit status reports so scripts can tell.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		fmt.Fprintf(os.Stderr, "published %s v%d\n", *path, doc.Version)
		os.Exit(exitNotSynced)
	}
	if err != nil {
		if errors.Is(err, storefmt.ErrNotModified) {
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
