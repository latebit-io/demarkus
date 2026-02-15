package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/latebit/demarkus/server/internal/config"
	"github.com/latebit/demarkus/server/internal/handler"
	devtls "github.com/latebit/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

func main() {
	root := flag.String("root", "", "content directory to serve (overrides DEMARKUS_ROOT)")
	port := flag.Int("port", 0, "port to listen on (overrides DEMARKUS_PORT)")
	flag.Parse()

	cfg, _ := config.NewConfig()

	// Flag overrides take precedence over env vars
	if *root != "" {
		cfg.ContentDir = *root
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if cfg.ContentDir == "" {
		log.Fatal("config: content directory is required (set DEMARKUS_ROOT or use -root flag)")
	}

	tlsConfig, err := devtls.GenerateDevConfig()
	if err != nil {
		log.Fatalf("generating TLS config: %v", err)
	}

	quicConfig := &quic.Config{
		MaxIncomingStreams:    int64(cfg.MaxStreams),
		MaxIncomingUniStreams: 0,
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := quic.ListenAddr(addr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	h := &handler.Handler{ContentDir: cfg.ContentDir}

	log.Printf("demarkus-server listening on %s (root: %s)", addr, cfg.ContentDir)

	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			log.Fatalf("accept connection: %v", err)
		}
		go handleConn(conn, h)
	}
}

func handleConn(conn *quic.Conn, h *handler.Handler) {
	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed
		}
		go h.HandleStream(stream)
	}
}
