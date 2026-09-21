package fetchtest

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
)

// Dispatcher is Client for callers whose List takes its options, as the
// broker's world dispatcher does. Same fields, so literals read alike.
type Dispatcher Client

func (d *Dispatcher) client() *Client { return (*Client)(d) }

// Fetch records and answers a FETCH.
func (d *Dispatcher) Fetch(host, path, token string) (fetch.Result, error) {
	return d.client().Fetch(host, path, token)
}

// FetchContext is Fetch, failing first when ctx is done.
func (d *Dispatcher) FetchContext(ctx context.Context, host, path, token string) (fetch.Result, error) {
	return d.client().FetchContext(ctx, host, path, token)
}

// FetchConditional records and answers a FETCHCONDITIONAL.
func (d *Dispatcher) FetchConditional(host, path, token, etag string) (fetch.Result, error) {
	return d.client().FetchConditional(host, path, token, etag)
}

// FetchConditionalContext is FetchConditional, failing first when ctx is done.
func (d *Dispatcher) FetchConditionalContext(ctx context.Context, host, path, token, etag string) (fetch.Result, error) {
	return d.client().FetchConditionalContext(ctx, host, path, token, etag)
}

// List records and answers a LIST.
func (d *Dispatcher) List(host, path, token string, opts fetch.ListOptions) (fetch.Result, error) {
	return d.client().ListWithOptions(host, path, token, opts)
}

// Versions records and answers a VERSIONS.
func (d *Dispatcher) Versions(host, path, token string) (fetch.Result, error) {
	return d.client().Versions(host, path, token)
}

// Lookup records and answers a LOOKUP.
func (d *Dispatcher) Lookup(host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	return d.client().Lookup(host, scope, query, token, opts)
}

// LookupContext is Lookup, failing first when ctx is done.
func (d *Dispatcher) LookupContext(ctx context.Context, host, scope, query, token string, opts fetch.LookupOptions) (fetch.Result, error) {
	return d.client().LookupContext(ctx, host, scope, query, token, opts)
}

// Publish records and answers a PUBLISH.
func (d *Dispatcher) Publish(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	return d.client().Publish(host, path, body, token, expectedVersion, meta)
}

// PublishContext is Publish, failing first when ctx is done.
func (d *Dispatcher) PublishContext(ctx context.Context, host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	return d.client().PublishContext(ctx, host, path, body, token, expectedVersion, meta)
}

// Append records and answers a APPEND.
func (d *Dispatcher) Append(host, path, body, token string, expectedVersion int, meta map[string]string) (fetch.Result, error) {
	return d.client().Append(host, path, body, token, expectedVersion, meta)
}

// Archive records and answers a ARCHIVE.
func (d *Dispatcher) Archive(host, path, token string) (fetch.Result, error) {
	return d.client().Archive(host, path, token)
}

// FetchCallCount is the number of FETCHes so far, safe during concurrent use.
func (d *Dispatcher) FetchCallCount() int { return d.client().FetchCallCount() }

// Calls copies the record under the lock, for reads that race the client.
func (d *Dispatcher) Calls() Calls { return d.client().Calls() }

// Lock guards direct reads of the call record while the client is in use.
func (d *Dispatcher) Lock() { d.client().Lock() }

// Unlock releases Lock.
func (d *Dispatcher) Unlock() { d.client().Unlock() }
