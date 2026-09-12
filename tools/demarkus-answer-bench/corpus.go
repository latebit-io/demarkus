package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/latebit-io/demarkus/tools/internal/answerbench"
)

func runCorpus(ctx context.Context, command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var opts answerbench.PackOptions
	flags.StringVar(&opts.Root, "root", "", "existing corpus for packing; new destination for restore")
	flags.StringVar(&opts.Archive, "archive", "", "private compressed corpus file")
	flags.StringVar(&opts.Manifest, "manifest", "", "versionable corpus manifest")
	if command == "corpus-pack" {
		flags.StringVar(&opts.ID, "id", "", "stable corpus identifier")
		flags.StringVar(&opts.Source, "source", "", "logical source URL")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s takes flags only; unexpected argument %q", command, flags.Arg(0))
	}
	if opts.Root == "" || opts.Archive == "" || opts.Manifest == "" {
		return fmt.Errorf("%s requires -root, -archive and -manifest", command)
	}
	var manifest answerbench.CorpusManifest
	var err error
	if command == "corpus-pack" {
		manifest, err = answerbench.PackCorpus(ctx, &opts)
	} else {
		manifest, err = answerbench.RestoreCorpus(ctx, opts.Manifest, opts.Archive, opts.Root)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}
