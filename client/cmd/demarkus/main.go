package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/url"
	"os"

	"github.com/latebit/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: demarkus mark://host:port/path\n")
		os.Exit(1)
	}

	u, err := url.Parse(os.Args[1])
	if err != nil {
		log.Fatalf("invalid URL: %v", err)
	}
	if u.Scheme != "mark" {
		log.Fatalf("unsupported scheme: %s (expected mark://)", u.Scheme)
	}

	host := u.Host
	if u.Port() == "" {
		host = fmt.Sprintf("%s:%d", u.Hostname(), protocol.DefaultPort)
	}

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{protocol.ALPN},
	}

	conn, err := quic.DialAddr(context.Background(), host, tlsConf, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatalf("open stream: %v", err)
	}

	verb := protocol.VerbFetch
	path := u.Path
	if path == "" || path[len(path)-1] == '/' {
		verb = protocol.VerbList
		if path == "" {
			path = "/"
		}
	}

	req := protocol.Request{Verb: verb, Path: path}
	if _, err := req.WriteTo(stream); err != nil {
		log.Fatalf("send request: %v", err)
	}
	// Close the write side (sends FIN) so the server knows the request is complete.
	stream.Close()

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		log.Fatalf("read response: %v", err)
	}

	fmt.Printf("[%s]", resp.Status)
	for k, v := range resp.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	fmt.Println()
	fmt.Print(resp.Body)
}
