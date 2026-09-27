package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/internal/tokens"
	"github.com/latebit-io/demarkus/protocol"
)

// watchMain prints one tab separated line per change hint until interrupted:
// cursor, op, path, version, hash, agent. A resync is a line whose op is
// resync; everything derived from earlier lines is stale from then on.
func watchMain(ctx context.Context, args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	since := fs.String("since", "", "resume after this cursor from an earlier watch")
	authToken := fs.String("auth", "", "auth token for a path under read authorisation (env: DEMARKUS_AUTH)")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus watch [-since CURSOR] [-auth TOKEN] [-insecure] mark://host:port/prefix/\n\n")
		fmt.Fprintf(os.Stderr, "Subscribe to change hints under a prefix (ending in /) or for one document and\n")
		fmt.Fprintf(os.Stderr, "print one line per change: cursor, op, path, version, hash, agent. The watch\n")
		fmt.Fprintf(os.Stderr, "reconnects on its own; a line whose op is resync means earlier state is stale.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}

	if fs.NArg() < 1 {
		fs.Usage()
		os.Exit(1)
	}
	var cursor protocol.Cursor
	if *since != "" {
		var err error
		if cursor, err = protocol.ParseCursor(*since); err != nil {
			log.Fatal(err)
		}
	}
	target := mustTarget(fs.Arg(0))
	host, path := target.DialHost(), target.Path
	token := tokens.Resolve(tokens.Credential{Explicit: *authToken, Origin: host}, host, tokens.LoadDefault())

	client := fetch.NewClient(fetch.Options{Insecure: *insecure})
	defer client.Close()

	watch, err := client.Watch(ctx, fetch.WatchRequest{Host: host, Path: path, Token: token, Since: cursor})
	if err != nil {
		log.Fatal(err)
	}
	defer watch.Close()

	out := bufio.NewWriter(os.Stdout)
	for {
		notice, err := watch.Next(ctx)
		if err != nil {
			if flushErr := out.Flush(); flushErr != nil {
				log.Print(flushErr)
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			log.Fatal(err)
		}
		line := notice.Cursor.String() + "\tresync\t\t\t\t"
		if !notice.Resync {
			e := notice.Event
			line = fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%s", e.Cursor, e.Op, e.Path, e.Version, e.Hash, e.Agent)
		}
		if _, err := out.WriteString(line + "\n"); err != nil {
			log.Fatal(err)
		}
		if err := out.Flush(); err != nil {
			log.Fatal(err)
		}
	}
}
