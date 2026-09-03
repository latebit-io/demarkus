// demarkus-memory-broker is memory as a service for MCP hosts: the
// OAuth-fronted MCP gateway that gives each identity a private,
// versioned personal memory. Built over the same shared broker libraries
// as demarkus-knowledge-broker with the memory authorization model:
// identity maps to exactly one world and reads AND writes are locked to
// it, with memory-template seeding, endpoint instructions and memory
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

	"github.com/latebit-io/demarkus/tools/internal/broker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/demarkus-memory-broker/config.yaml", "path to broker YAML config")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (default: in-cluster config)")
	showVersion := flag.Bool("version", false, "print version and exit")
	deprovision := flag.String("deprovision-tenant", "", "deprovision the named tenant world and exit (operator flow, run on a user's deletion request)")
	deleteBucket := flag.Bool("delete-bucket", false, "with -deprovision-tenant: also permanently delete the tenant's GCS bucket and data")
	flag.Parse()

	if *showVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if *deprovision != "" {
		found, err := broker.RunDeprovision(*configPath, *kubeconfig, *deprovision, *deleteBucket, log)
		if err != nil {
			log.Error("deprovision failed", "world", *deprovision, "err", err)
			os.Exit(1)
		}
		if !found {
			// The documented rerun-to-converge path: cleanup completed.
			log.Warn("tenant absent from registry; cleanup converged", "world", *deprovision)
		}
		return
	}
	if *deleteBucket {
		log.Error("-delete-bucket requires -deprovision-tenant")
		os.Exit(1)
	}

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
	buckets, closeClient, err := broker.NewGCSBuckets(cfg, log)
	if err != nil {
		return nil, nil, fmt.Errorf("provisioning enabled: %w", err)
	}
	provisioner := srv.EnableProvisioning(buckets)
	log.Info("memory broker: provisioning enabled",
		"mode", cfg.Provisioning.Mode,
		"maxTenants", cfg.Provisioning.MaxTenants,
		"authorityDomain", cfg.Provisioning.AuthorityDomain)
	return []func(context.Context){provisioner.RunRegistrySync}, closeClient, nil
}
