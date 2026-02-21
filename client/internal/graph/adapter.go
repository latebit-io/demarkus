package graph

// ClientFetcher adapts any client with a Fetch(host, path) method that returns
// a status and body into the graph.Fetcher interface. This avoids a direct
// dependency on the fetch package.
type ClientFetcher struct {
	// FetchFunc performs a document fetch and returns (status, body, error).
	FetchFunc func(host, path string) (status, body string, err error)
}

// Fetch implements the Fetcher interface.
func (a *ClientFetcher) Fetch(host, path string) (FetchResult, error) {
	status, body, err := a.FetchFunc(host, path)
	if err != nil {
		return FetchResult{}, err
	}
	return FetchResult{Status: status, Body: body}, nil
}
