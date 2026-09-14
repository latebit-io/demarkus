package main

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/graph"
	"github.com/latebit-io/demarkus/client/graphstore"
)

func (h *handler) graphFetch(ctx context.Context, host, path string) (graph.FetchResult, error) {
	r, err := h.client.FetchContext(ctx, host, path, h.resolveToken(host))
	if err != nil {
		return graph.FetchResult{}, err
	}
	return graph.FetchResult{Status: r.Response.Status, Body: r.Response.Body, Metadata: r.Response.Metadata}, nil
}

func (h *handler) revalidateBacklinks(ctx context.Context, url string) string {
	if h.graphStore == nil || h.client == nil {
		return ""
	}
	urls := h.graphStore.Backlinks(url)
	if len(urls) == 0 {
		return h.graphStore.FreshnessSummary() + "\n"
	}
	result, err := h.graphStore.Revalidate(ctx, urls, h.graphFetch, fetch.ParseMarkURL)
	return graphstore.ValidationSummary(result, err) + h.graphStore.FreshnessSummary() + "\n"
}

var _ graphstore.FetchFunc = (*handler)(nil).graphFetch
