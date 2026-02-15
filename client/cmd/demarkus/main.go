package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/protocol"
	"github.com/quic-go/quic-go"
)

func main() {
	verb := flag.String("X", protocol.VerbFetch, "request verb (FETCH, LIST)")
	noCache := flag.Bool("no-cache", false, "disable caching")
	cacheDir := flag.String("cache-dir", defaultCacheDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: demarkus [-X VERB] mark://host:port/path\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	*verb = strings.ToUpper(*verb)
	if err := validateVerb(*verb); err != nil {
		log.Fatal(err)
	}

	u, err := url.Parse(flag.Arg(0))
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

	path := u.Path
	if path == "" {
		path = "/"
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

	c := cache.New(*cacheDir)

	req := protocol.Request{Verb: *verb, Path: path, Metadata: make(map[string]string)}

	// Populate conditional request metadata from cache.
	var cached *cache.Entry
	if !*noCache {
		cached, _ = c.Get(host, path, *verb)
		if cached != nil {
			if etag := cached.Response.Metadata["etag"]; etag != "" {
				req.Metadata["if-none-match"] = etag
			}
			if mod := cached.Response.Metadata["modified"]; mod != "" {
				req.Metadata["if-modified-since"] = mod
			}
		}
	}

	if _, err := req.WriteTo(stream); err != nil {
		log.Fatalf("send request: %v", err)
	}
	// Close the write side (sends FIN) so the server knows the request is complete.
	stream.Close()

	resp, err := protocol.ParseResponse(stream)
	if err != nil {
		log.Fatalf("read response: %v", err)
	}

	// On not-modified, use the cached response.
	if resp.Status == protocol.StatusNotModified && cached != nil {
		resp = cached.Response
	}

	// Cache successful responses.
	if !*noCache && resp.Status == protocol.StatusOK {
		if err := c.Put(host, path, *verb, resp); err != nil {
			log.Printf("cache write: %v", err)
		}
	}

	fmt.Printf("[%s]", resp.Status)
	for k, v := range resp.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	fmt.Println()
	fmt.Print(resp.Body)
}

var validVerbs = map[string]bool{
	protocol.VerbFetch: true,
	protocol.VerbList:  true,
}

func validateVerb(verb string) error {
	if !validVerbs[verb] {
		return fmt.Errorf("unsupported verb: %s (valid: FETCH, LIST)", verb)
	}
	return nil
}

func defaultCacheDir() string {
	if dir := os.Getenv("DEMARKUS_CACHE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".mark", "cache")
	}
	return filepath.Join(home, ".mark", "cache")
}
