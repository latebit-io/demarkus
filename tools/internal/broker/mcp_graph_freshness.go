package broker

import (
	"context"
)

func (g *mcpGateway) revalidateBacklinks(ctx context.Context, state *gatewayGraph, url string) string {
	return state.graphStore.RevalidateBacklinks(ctx, url, g.crawlFetchFn(ctx))
}
