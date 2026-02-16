package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/latebit/demarkus/client/internal/cache"
	"github.com/latebit/demarkus/client/internal/fetch"
	"github.com/latebit/demarkus/protocol"
)

func main() {
	verb := flag.String("X", protocol.VerbFetch, "request verb (FETCH, LIST)")
	noCache := flag.Bool("no-cache", false, "disable caching")
	insecure := flag.Bool("insecure", false, "skip TLS certificate verification")
	cacheDir := flag.String("cache-dir", cache.DefaultDir(), "cache directory (env: DEMARKUS_CACHE_DIR)")
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

	host, path, err := fetch.ParseMarkURL(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	opts := fetch.Options{Insecure: *insecure}
	if !*noCache {
		opts.Cache = cache.New(*cacheDir)
	}

	client := fetch.NewClient(opts)
	defer client.Close()

	result, err := client.Fetch(host, path, *verb)
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
