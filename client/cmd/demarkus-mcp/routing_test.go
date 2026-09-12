package main

import (
	"testing"

	"github.com/latebit-io/demarkus/client/fetch"
)

func TestRouteDefaultHostPreservesIdentity(t *testing.T) {
	var opts fetch.Options
	if err := routeDefaultHost(&opts, "mark://soul.example", "127.0.0.1:16319"); err != nil {
		t.Fatal(err)
	}
	route, ok := opts.Endpoints["soul.example:6309"]
	if !ok || route.DialAddress != "127.0.0.1:16319" || route.ServerName != "soul.example" {
		t.Fatalf("unexpected route: %+v", opts.Endpoints)
	}
	for _, address := range []string{"localhost", ":16319", "localhost:0", "localhost:65536"} {
		if err := routeDefaultHost(&opts, "mark://soul.example", address); err == nil {
			t.Errorf("invalid address accepted: %s", address)
		}
	}
	if err := routeDefaultHost(&opts, "", "localhost:16319"); err == nil {
		t.Fatal("route without logical host accepted")
	}
}
