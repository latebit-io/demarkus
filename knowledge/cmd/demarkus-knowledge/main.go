// demarkus-knowledge is the knowledge server and the broker in one process:
// the worlds it serves are dispatched to the MCP gateway in process, every
// other configured world over QUIC. The two configurations stay separate
// until they merge; the profile picks the knowledge or the memory surface.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/latebit-io/demarkus/knowledge/internal/broker"
	"github.com/latebit-io/demarkus/server/knowledgeserver"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("demarkus-knowledge", flag.ContinueOnError)
	serverConfig := flags.String("server-config", "", "path to the knowledge server's multi-world YAML configuration")
	brokerConfig := flags.String("broker-config", "", "path to the broker YAML configuration")
	profile := flags.String("profile", "knowledge", "gateway surface: knowledge or memory")
	kubeconfig := flags.String("kubeconfig", "", "path to kubeconfig (default: in-cluster config)")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *serverConfig == "" || *brokerConfig == "" {
		return errors.New("-server-config and -broker-config are required")
	}
	opts, err := brokerOptions(*profile, *kubeconfig)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	server, err := knowledgeserver.Open(knowledgeserver.Options{ConfigFile: *serverConfig, Logger: log})
	if err != nil {
		return err
	}
	defer server.Close()
	opts.LocalWorlds = server
	b, err := broker.Open(*brokerConfig, opts, log)
	if err != nil {
		return err
	}
	defer b.Close()

	// The broker stops first so no tool call reaches a draining world; the
	// server stops once the broker has drained. A server that dies on its
	// own takes the broker down with it.
	brokerCtx, stopBroker := server.WatchSignals(context.Background())
	defer stopBroker()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() {
		err := server.Serve(serverCtx)
		serverDone <- err
		stopBroker()
	}()

	brokerErr := b.Serve(brokerCtx)
	stopServer()
	serverErr := <-serverDone
	return errors.Join(brokerErr, serverErr)
}

// brokerOptions is the product surface, selected by profile.
func brokerOptions(profile, kubeconfig string) (*broker.RunOptions, error) {
	switch profile {
	case "knowledge":
		return broker.KnowledgeOptions(version, kubeconfig), nil
	case "memory":
		return broker.MemoryOptions(version, kubeconfig), nil
	default:
		return nil, fmt.Errorf("unknown profile %q: want knowledge or memory", profile)
	}
}
