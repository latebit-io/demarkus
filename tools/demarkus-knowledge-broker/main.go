// demarkus-knowledge-broker is the OIDC token broker that exchanges a
// verified identity for demarkus world access: SSO is the org gate,
// reads are open, and writes use a long-lived per-world token the
// broker provisions and holds. It serves the browser code flow, the
// RFC 8628 device flow, broker-signed refresh tokens with RFC 7009
// revoke, per-subject and per-IP rate limiting, a leader-elected
// refresh-token sweeper, GET /me/install, and the always-on
// MCP-over-HTTPS gateway at /mcp. Process lifecycle is the shared
// broker.Run; this main only names the product.
package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/latebit-io/demarkus/tools/internal/broker"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/demarkus-knowledge-broker/config.yaml", "path to broker YAML config")
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
		LogName:        "broker",
		Realm:          "demarkus-knowledge-broker",
		Profile:        broker.KnowledgeGatewayProfile(),
		Version:        version,
		KubeconfigPath: *kubeconfig,
	}, log)
	if err != nil {
		log.Error("broker exited with error", "err", err)
		os.Exit(1)
	}
}
