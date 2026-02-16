package main

import (
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
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

	var c *cache.Cache
	if !*noCache {
		c = cache.New(*cacheDir)
	}

	result, err := fetch.Fetch(host, path, *verb, c)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("[%s]", result.Response.Status)
	for k, v := range result.Response.Metadata {
		fmt.Printf(" %s=%s", k, v)
	}
	if result.FromCache {
		fmt.Printf(" (cached)")
	}
	fmt.Println()
	fmt.Print(result.Response.Body)
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
