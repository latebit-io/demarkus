package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/protocol"
)

// pingMain reports whether a server is serving: it fetches the always
// public manifest path and exits 0 when the server answered, manifest or
// not. Probes use it; a world without a manifest is not an outage.
func pingMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus ping [-insecure] mark://host:port\n\n")
		fmt.Fprintf(os.Stderr, "Exit 0 when the server answers a request, 1 when it does not.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	host, err := links.DialHost(fs.Arg(0))
	if err != nil {
		log.Fatalf("invalid URL: %v", err)
	}

	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	result, err := client.Fetch(ctx, fetch.FetchRequest{Host: host, Path: protocol.WellKnownManifestPath})
	if err != nil {
		log.Fatal(err)
	}
	status := result.Response.Status
	fmt.Println(status)
	if code := pingExitCode(status); code != 0 {
		fmt.Fprintf(os.Stderr, "demarkus: %s\n", status)
		os.Exit(code)
	}
}

// pingExitCode treats any answer about the manifest as a live server;
// not-found only says nobody published one.
func pingExitCode(status string) int {
	if status == protocol.StatusNotFound {
		return 0
	}
	return exitCodeForStatus(status)
}
