package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob/gcs"
	"github.com/latebit-io/demarkus/server/internal/knowledgeconfig"
)

const (
	maxObjectBytes = 4 << 20
	commandTimeout = 2 * time.Minute
)

var version = "dev"

type config struct {
	bucketName string
	worldID    string
	policy     []byte
	version    bool
}

type storageClientFactory func(context.Context) (*storage.Client, error)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, func(ctx context.Context) (*storage.Client, error) {
		return storage.NewClient(ctx)
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, output io.Writer, newClient storageClientFactory) error {
	config, err := loadConfig(arguments)
	if err != nil {
		return err
	}
	if config.version {
		_, err := fmt.Fprintln(output, version)
		return err
	}

	client, err := newClient(ctx)
	if err != nil {
		return fmt.Errorf("create GCS client with application default credentials: %w", err)
	}
	objects, setupErr := gcs.New(client, config.bucketName, maxObjectBytes)
	if setupErr == nil {
		setupErr = bootstrap(ctx, objects, config.worldID, config.policy)
	}
	closeErr := client.Close()
	if setupErr != nil || closeErr != nil {
		return errors.Join(setupErr, wrapCloseError(closeErr))
	}
	if _, err := fmt.Fprintf(output, "bootstrapped and verified gs://%s\n", config.bucketName); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func loadConfig(arguments []string) (config, error) {
	flags := flag.NewFlagSet("demarkus-knowledge-bootstrap", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bucketURL := flags.String("bucket", "", "target GCS bucket as gs://bucket")
	worldID := flags.String("world-id", "", "canonical lowercase world UUID")
	policyFile := flags.String("policy-file", "", "local policy markdown file")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return config{}, err
	}
	if flags.NArg() != 0 {
		return config{}, fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *showVersion {
		return config{version: true}, nil
	}
	bucketName, err := knowledgeconfig.ParseBucketURL(*bucketURL)
	if err != nil {
		return config{}, fmt.Errorf("invalid -bucket: %w", err)
	}
	if !knowledgeconfig.ValidWorldID(*worldID) {
		return config{}, fmt.Errorf("invalid -world-id: must be a canonical lowercase RFC 4122 UUID (got %q)", *worldID)
	}
	if *policyFile == "" {
		return config{}, errors.New("-policy-file is required")
	}
	policy, err := readPolicy(*policyFile)
	if err != nil {
		return config{}, err
	}
	if err := validatePolicy(policy); err != nil {
		return config{}, err
	}
	return config{bucketName: bucketName, worldID: *worldID, policy: policy}, nil
}

func readPolicy(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open -policy-file: %w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return nil, errors.Join(fmt.Errorf("stat -policy-file: %w", statErr), wrapFileError("close", file.Close()))
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New("-policy-file must be a regular file"), wrapFileError("close", file.Close()))
	}
	policy, readErr := io.ReadAll(io.LimitReader(file, protocol.MaxBodyLength+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(wrapFileError("read", readErr), wrapFileError("close", closeErr))
	}
	if len(policy) > protocol.MaxBodyLength {
		return nil, fmt.Errorf("-policy-file exceeds %d bytes", protocol.MaxBodyLength)
	}
	return policy, nil
}

func wrapCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close GCS client: %w", err)
}

func wrapFileError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s -policy-file: %w", operation, err)
}
