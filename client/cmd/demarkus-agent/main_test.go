package main

import (
	"testing"

	"github.com/latebit-io/demarkus/client/internal/fedcrawl"
)

func TestBlockIncompletePublication(t *testing.T) {
	incomplete := &fedcrawl.CrawlResult{Incomplete: true}
	completeWithErrors := &fedcrawl.CrawlResult{Errors: []string{"state save failed"}}

	if !blockIncompletePublication(incomplete, true, false, 1) {
		t.Fatal("incomplete crawl with a hub should block publication")
	}
	if blockIncompletePublication(incomplete, true, false, 0) {
		t.Fatal("zero configured hubs should not block")
	}
	if blockIncompletePublication(completeWithErrors, true, true, 1) {
		t.Fatal("non-inventory errors should not block publication")
	}
}
