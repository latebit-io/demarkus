package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/latebit/demarkus/protocol"
	"github.com/latebit/demarkus/server/internal/handler"
	devtls "github.com/latebit/demarkus/server/internal/tls"
	"github.com/quic-go/quic-go"
)

func main() {
	root := flag.String("root", ".", "content directory to serve")
	port := flag.Int("port", protocol.DefaultPort, "port to listen on")
	flag.Parse()

	tlsConfig, err := devtls.GenerateDevConfig()
	if err != nil {
		log.Fatalf("generating TLS config: %v", err)
	}

	addr := fmt.Sprintf(":%d", *port)
	listener, err := quic.ListenAddr(addr, tlsConfig, nil)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	h := &handler.Handler{ContentDir: *root}

	log.Printf("demarkus-server listening on %s (root: %s)", addr, *root)

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
