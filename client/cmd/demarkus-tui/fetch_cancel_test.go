package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"

	"github.com/latebit-io/demarkus/client/fetch"
)

// blockingFetcher holds every FETCH until its context ends and reports why.
type blockingFetcher struct{ started chan string }

func (b blockingFetcher) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	b.started <- r.Path
	<-ctx.Done()
	return fetch.Result{}, ctx.Err()
}

// A fetch the user has navigated away from is cancelled, not left to run out
// its retries against a slow server while its result would be dropped anyway.
func TestSupersededFetchIsCancelled(t *testing.T) {
	supersede := map[string]func(m *model){
		"another fetch":  func(m *model) { m.startFetch("mark://example.com/second.md") },
		"bookmarks view": func(m *model) { m.cancelFetch() },
	}
	for name, act := range supersede {
		t.Run(name, func(t *testing.T) {
			fetcher := blockingFetcher{started: make(chan string, 2)}
			m := model{fetcher: fetcher, addressBar: textinput.New()}
			first := m.startFetch("mark://example.com/first.md")
			firstSeq := m.fetchSeq

			done := make(chan fetchResult, 1)
			go func() { done <- first().(fetchResult) }()
			if got := <-fetcher.started; got != "/first.md" {
				t.Fatalf("first fetch path = %q", got)
			}

			act(&m)
			select {
			case res := <-done:
				if !errors.Is(res.err, context.Canceled) {
					t.Fatalf("superseded fetch err = %v, want context.Canceled", res.err)
				}
				if res.seq != firstSeq || res.seq == m.fetchSeq {
					t.Fatalf("result seq = %d, model seq = %d: a superseded result must read as stale", res.seq, m.fetchSeq)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("superseded fetch kept running")
			}
		})
	}
}

// The fetch that is still wanted keeps its context when its result arrives.
func TestCurrentFetchResultReleasesItsContext(t *testing.T) {
	fetcher := blockingFetcher{started: make(chan string, 1)}
	m := model{fetcher: fetcher, addressBar: textinput.New()}
	m.startFetch("mark://example.com/doc.md")
	if m.fetchCancel == nil {
		t.Fatal("startFetch kept no cancel func")
	}
	out, _ := m.handleFetchResult(fetchResult{err: errors.New("boom"), seq: m.fetchSeq})
	if out.(model).fetchCancel != nil {
		t.Error("a delivered result must release the fetch context")
	}
}

// followLink returns the model by value; the fetch it starts must be the one
// that model can cancel, or the next navigation supersedes nothing.
func TestFollowLinkKeepsTheFetchItStarted(t *testing.T) {
	fetcher := blockingFetcher{started: make(chan string, 1)}
	m := model{fetcher: fetcher, addressBar: textinput.New()}
	out, cmd := m.followLink("mark://example.com/next.md")
	next := out.(model)
	if cmd == nil || next.fetchCancel == nil || next.fetchSeq != m.fetchSeq+1 {
		t.Fatalf("followLink: cmd nil=%v, cancel nil=%v, seq %d -> %d", cmd == nil, next.fetchCancel == nil, m.fetchSeq, next.fetchSeq)
	}
	done := make(chan fetchResult, 1)
	go func() { done <- cmd().(fetchResult) }()
	<-fetcher.started
	next.cancelFetch()
	select {
	case res := <-done:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", res.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the returned model could not cancel the fetch followLink started")
	}
}
