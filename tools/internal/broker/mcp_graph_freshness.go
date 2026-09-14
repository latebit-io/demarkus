package broker

import (
	"context"

	"github.com/latebit-io/demarkus/client/graphstore"
)

func (g *mcpGateway) revalidateBacklinks(ctx context.Context, state *gatewayGraph, url string) string {
	urls := state.graphStore.Backlinks(url)
	if len(urls) == 0 {
		return state.graphStore.FreshnessSummary() + "\n"
	}
	result, err := state.graphStore.Revalidate(ctx, urls, g.crawlFetchFn(ctx), brokerCrawlParseURL)
	return graphstore.ValidationSummary(result, err) + state.graphStore.FreshnessSummary() + "\n"
}
