package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/server/knowledgeexport"
	"github.com/latebit-io/demarkus/tools/internal/answerbench"
)

func runGCSPack(ctx context.Context, args []string) (err error) {
	flags := flag.NewFlagSet("corpus-pack-gcs", flag.ContinueOnError)
	var opts answerbench.PackOptions
	bucket := flags.String("bucket", "", "GCS bucket name")
	worldID := flags.String("world-id", "", "immutable world UUID")
	flags.StringVar(&opts.Archive, "archive", "", "private compressed corpus file")
	flags.StringVar(&opts.Manifest, "manifest", "", "versionable corpus manifest")
	flags.StringVar(&opts.ID, "id", "", "stable corpus identifier")
	flags.StringVar(&opts.Source, "source", "", "logical source URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("corpus-pack-gcs takes flags only; unexpected argument %q", flags.Arg(0))
	}
	if *bucket == "" || *worldID == "" || opts.Archive == "" || opts.Manifest == "" || opts.ID == "" || opts.Source == "" {
		return errors.New("corpus-pack-gcs requires -bucket, -world-id, -archive, -manifest, -id and -source")
	}
	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("create GCS client: %w", err)
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	exporter, err := knowledgeexport.New(client, *bucket, *worldID)
	if err != nil {
		return err
	}
	manifest, err := answerbench.PackExport(ctx, &opts, exporter)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(manifest)
}
