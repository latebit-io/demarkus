package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/latebit/demarkus/server/internal/config"
	"github.com/latebit/demarkus/server/internal/handler"
	servertls "github.com/latebit/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

func main() {
	root := flag.String("root", "", "content directory to serve (overrides DEMARKUS_ROOT)")
	port := flag.Int("port", 0, "port to listen on (overrides DEMARKUS_PORT)")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate PEM file (overrides DEMARKUS_TLS_CERT)")
	tlsKey := flag.String("tls-key", "", "path to TLS private key PEM file (overrides DEMARKUS_TLS_KEY)")
	flag.Parse()

	cfg, err := config.NewConfig()
	if err != nil {
		log.Printf("[WARN] config: %v", err)
	}

	// Flag overrides take precedence over env vars
	if *root != "" {
		cfg.ContentDir = *root
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if *tlsCert != "" {
		cfg.TLSCert = *tlsCert
	}
	if *tlsKey != "" {
		cfg.TLSKey = *tlsKey
	}
	if cfg.ContentDir == "" {
		log.Fatal("[ERROR] content directory is required (set DEMARKUS_ROOT or use -root flag)")
	}

	tlsConfig, err := loadTLS(cfg)
	if err != nil {
		log.Fatalf("[ERROR] %v", err)
	}

	quicConfig := &quic.Config{
		MaxIncomingStreams:    int64(cfg.MaxStreams),
		MaxIncomingUniStreams: 0,
		MaxIdleTimeout:        cfg.IdleTimeout,
	}

	addr := fmt.Sprintf(":%d", cfg.Port)
	listener, err := quic.ListenAddr(addr, tlsConfig, quicConfig)
	if err != nil {
		log.Fatalf("[ERROR] listen on %s: %v", addr, err)
	}
	defer listener.Close()

	h := &handler.Handler{ContentDir: cfg.ContentDir}

	log.Printf("[INFO] demarkus-server listening on %s (root: %s, idle_timeout: %v, request_timeout: %v)",
		addr, cfg.ContentDir, cfg.IdleTimeout, cfg.RequestTimeout)

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Accept connections in a goroutine so we can listen for shutdown signals
	errChan := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				errChan <- err
				return
			}
			go handleConn(conn, h)
		}
	}()

	// Wait for shutdown signal or listener error
	select {
	case sig := <-sigChan:
		log.Printf("[INFO] received signal %v, initiating graceful shutdown", sig)
	case err := <-errChan:
		log.Printf("[ERROR] listener error: %v", err)
	}

	// Close the listener to stop accepting new connections
	listener.Close()
	log.Printf("[INFO] demarkus-server stopped")
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

// loadTLS returns a TLS config based on the server configuration.
// If TLSCert and TLSKey are set, loads certificates from disk (production mode).
// If neither is set, generates a self-signed dev certificate.
// Returns an error if only one of cert/key is provided.
func loadTLS(cfg *config.Config) (*tls.Config, error) {
	haveCert := cfg.TLSCert != ""
	haveKey := cfg.TLSKey != ""

	switch {
	case haveCert && haveKey:
		log.Printf("[INFO] tls: loading certificate from %s", cfg.TLSCert)
		return servertls.LoadConfig(cfg.TLSCert, cfg.TLSKey)
	case haveCert != haveKey:
		return nil, fmt.Errorf("both -tls-cert and -tls-key must be provided (got cert=%q, key=%q)", cfg.TLSCert, cfg.TLSKey)
	default:
		log.Printf("[INFO] tls: using self-signed dev certificate (set DEMARKUS_TLS_CERT and DEMARKUS_TLS_KEY for production)")
		return servertls.GenerateDevConfig()
	}
}
