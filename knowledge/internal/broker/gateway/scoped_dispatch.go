package gateway

import (
	"context"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/knowledge/internal/broker/core"
)

// scopedDispatcher serves only the worlds a gateway's profile view holds.
// The shared dispatcher knows every world in the process, memory tenants
// included; without this, a knowledge caller could name one by URL.
type scopedDispatcher struct {
	view  *core.WorldView
	inner WorldDispatcher
}

func scopeDispatcher(view *core.WorldView, inner WorldDispatcher) WorldDispatcher {
	if view == nil || inner == nil {
		return inner
	}
	return &scopedDispatcher{view: view, inner: inner}
}

func (s *scopedDispatcher) check(worldName string) error {
	return inView(s.view, worldName)
}

func inView(view *core.WorldView, worldName string) error {
	if _, ok := view.Find(worldName); !ok {
		return &errWorldNotFound{worldName: worldName}
	}
	return nil
}

// scopedWatcher watches only the worlds a gateway's profile view holds, as
// scopedDispatcher reads only them.
type scopedWatcher struct {
	view  *core.WorldView
	inner WorldWatcher
}

func scopeWatcher(view *core.WorldView, inner WorldWatcher) WorldWatcher {
	return &scopedWatcher{view: view, inner: inner}
}

func (s *scopedWatcher) Watch(ctx context.Context, r fetch.WatchRequest) (*fetch.Watch, error) {
	if err := inView(s.view, r.Host); err != nil {
		return nil, err
	}
	return s.inner.Watch(ctx, r)
}

func (s *scopedDispatcher) Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Fetch(ctx, r)
}

func (s *scopedDispatcher) List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.List(ctx, r)
}

func (s *scopedDispatcher) Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Versions(ctx, r)
}

func (s *scopedDispatcher) Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Lookup(ctx, r)
}

func (s *scopedDispatcher) Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Publish(ctx, r)
}

func (s *scopedDispatcher) Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Append(ctx, r)
}

func (s *scopedDispatcher) Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error) {
	if err := s.check(r.Host); err != nil {
		return fetch.Result{}, err
	}
	return s.inner.Archive(ctx, r)
}
