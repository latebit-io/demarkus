// demarkus-memory-broker is memory as a service for MCP hosts: the
// OAuth-fronted MCP gateway that gives each identity a private,
// versioned personal soul. Built over the same shared broker libraries
// as demarkus-knowledge-broker with the memory authorization model:
// identity maps to exactly one world and reads AND writes are locked to
// it, with soul-template seeding, endpoint instructions and soul
// prompts, and (when enabled) dynamic tenant provisioning behind the
// static | allowlisted | open gate. Process lifecycle is the shared
// broker.Run; this main wires the memory profile and provisioning.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"cloud.google.com/go/storage"

	"github.com/latebit-io/demarkus/tools/internal/broker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/demarkus-memory-broker/config.yaml", "path to broker YAML config")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster config)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	err := broker.Run(*configPath, &broker.RunOptions{
		LogName: "memory broker",
		Realm:   "demarkus-memory-broker",
		Profile: broker.MemoryGatewayProfile(),
		// Memory-broker invariants on top of the shared validation:
		// every static world names its tenant, and the provisioning
		// block, when enabled, is complete.
		Validate: []func(*broker.Config) error{
			(*broker.Config).ValidateTenantWorlds,
			(*broker.Config).ValidateProvisioning,
		},
		Setup:          setupProvisioning,
		Version:        version,
		KubeconfigPath: *kubeconfig,
	}, log)
	if err != nil {
		log.Error("memory broker exited with error", "err", err)
		os.Exit(1)
	}
}

// setupProvisioning wires the GCS bucket creator and dynamic tenant
// provisioning when the config asks for it: the registry sync keeps this
// replica converged with tenants provisioned by its siblings.
func setupProvisioning(cfg *broker.Config, srv *broker.Server, log *slog.Logger) (background []func(context.Context), cleanup func(), err error) {
	if !cfg.Provisioning.Enabled() {
		return nil, func() {}, nil
	}
	gcsClient, err := storage.NewClient(context.Background())
	if err != nil {
		return nil, nil, fmt.Errorf("provisioning enabled but GCS client unavailable: %w", err)
	}
	closeClient := func() {
		if closeErr := gcsClient.Close(); closeErr != nil {
			log.Warn("memory broker: GCS client close failed", "err", closeErr)
		}
	}
	buckets, err := broker.NewGCSBucketCreator(gcsClient, cfg.Provisioning.BucketProject, cfg.Provisioning.BucketLocation, log)
	if err != nil {
		closeClient()
		return nil, nil, err
	}
	provisioner := srv.EnableProvisioning(buckets)
	log.Info("memory broker: provisioning enabled",
		"mode", cfg.Provisioning.Mode,
		"maxTenants", cfg.Provisioning.MaxTenants,
		"authorityDomain", cfg.Provisioning.AuthorityDomain)
	return []func(context.Context){provisioner.RunRegistrySync}, closeClient, nil
}
