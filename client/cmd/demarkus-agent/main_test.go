package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/client/internal/fedcrawl"
)

func TestBlockIncompletePublication(t *testing.T) {
	incomplete := &fedcrawl.CrawlResult{Incomplete: true}
	completeWithErrors := &fedcrawl.CrawlResult{Errors: []string{"state save failed"}}

	if !blockIncompletePublication(incomplete, true, 1) {
		t.Fatal("incomplete crawl with a hub should block publication")
	}
	if blockIncompletePublication(incomplete, true, 0) {
		t.Fatal("zero configured hubs should not block")
	}
	if blockIncompletePublication(incomplete, false, 1) {
		t.Fatal("a crawl that publishes nothing should not block")
	}
	if blockIncompletePublication(completeWithErrors, true, 1) {
		t.Fatal("non-inventory errors should not block publication")
	}
}

// The hash index flags left with ADR 0035; crawl and daemon share the set.
func TestRemovedPublishFlagsAreUnknown(t *testing.T) {
	for _, name := range []string{"publish", "per-server"} {
		fs, _ := newCrawlFlagSet("crawl", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if err := fs.Parse([]string{"-" + name}); err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("-%s: err = %v, want an unknown flag", name, err)
		}
	}
	fs, f := newCrawlFlagSet("daemon", flag.ContinueOnError)
	if err := fs.Parse([]string{"-publish-graph", "-publish-retention", "5"}); err != nil || !f.publishGraph || f.publishRetention != 5 {
		t.Errorf("graph flags: err = %v, flags = %+v", err, f)
	}
}
