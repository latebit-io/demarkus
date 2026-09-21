// Package marktools owns the bodies of the mark_* MCP tools, so the direct
// client server and the broker gateway cannot drift apart. A surface parses
// arguments, supplies hooks for what differs, and wraps the Result.
package marktools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
)

// Backend is the protocol client every tool runs on. Host in a request is
// whatever Resolve returned: host:port for a direct client, a world name
// behind the broker.
type Backend interface {
	Fetch(ctx context.Context, r fetch.FetchRequest) (fetch.Result, error)
	List(ctx context.Context, r fetch.ListRequest) (fetch.Result, error)
	Versions(ctx context.Context, r fetch.VersionsRequest) (fetch.Result, error)
	Lookup(ctx context.Context, r fetch.LookupRequest) (fetch.Result, error)
	Publish(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Append(ctx context.Context, r fetch.WriteRequest) (fetch.Result, error)
	Archive(ctx context.Context, r fetch.ArchiveRequest) (fetch.Result, error)
}

// Target is a tool URL resolved by the surface.
type Target struct {
	Host string // the Backend's name for the server
	Path string
	// NodeURL is how the surface names the document: the graph key, and what
	// a tool prints. Identity for a direct client, the world URL for the broker.
	NodeURL string
	// Authority names the server in a published index: mark://host by identity,
	// or the world URL behind the broker.
	Authority string
}

// Site names the call that failed, for ErrText.
type Site string

// Sites, one per backend call a tool makes.
const (
	SiteVersions Site = "versions"
	SiteList     Site = "list"
	SiteLookup   Site = "lookup"
	SiteDiscover Site = "discover"
	SiteArchive  Site = "archive"
	SiteFetch    Site = "fetch"
	SitePublish  Site = "publish"
	SiteAppend   Site = "append"
	// SiteResolveIndex is mark_resolve reading its index.
	SiteResolveIndex Site = "resolve index"
)

// Hooks are what differs between surfaces. Resolve is required.
type Hooks struct {
	// Resolve turns a tool's url argument into a Target. Its error is shown
	// after "invalid URL: " unless it is Verbatim. An empty url asks for the
	// surface's default server, which a surface without one refuses.
	Resolve func(ctx context.Context, raw string) (Target, error)
	// ReadToken is the token a read sends to host; nil sends none.
	ReadToken func(ctx context.Context, host string) string
	// Writer authorizes verb on target and returns how to run the write. Its
	// error is the tool's answer as is. Nil means the surface cannot write.
	Writer func(ctx context.Context, target Target, verb string) (WriteFunc, error)
	// Agent names who writes; it becomes the agent metadata key, which a caller
	// cannot override. Nil writes none.
	Agent func(ctx context.Context) string
	// Graph is the graph store a call works on; nil or an error means none,
	// and the error's text is the tool's answer.
	Graph func(ctx context.Context) (*GraphScope, error)
	// Seen dedups full bodies mark_fetch already returned; nil never dedups.
	Seen SeenStore
	// ErrText words a failed backend call; nil says "<site> failed: <err>".
	ErrText func(site Site, host string, err error) string
	// Now stamps a published index; nil is time.Now.
	Now func() time.Time
	// Warnf reports a best effort step that failed; nil is log.Printf.
	Warnf func(format string, args ...any)
}

// Tools runs the mark_* tools over one Backend.
type Tools struct {
	backend Backend
	hooks   Hooks
}

// New refuses a nil backend and a missing Resolve hook: every tool needs both.
func New(backend Backend, hooks Hooks) (*Tools, error) {
	if backend == nil {
		return nil, errors.New("marktools: backend is required")
	}
	if hooks.Resolve == nil {
		return nil, errors.New("marktools: Resolve hook is required")
	}
	return &Tools{backend: backend, hooks: hooks}, nil
}

// Result is a tool's answer: the text, and whether it reports a failure.
type Result struct {
	Text    string
	IsError bool
}

func text(s string) Result { return Result{Text: s} }

func failure(format string, args ...any) Result {
	return Result{Text: fmt.Sprintf(format, args...), IsError: true}
}

// resolve returns the target, or the failure to answer with.
func (t *Tools) resolve(ctx context.Context, raw string) (Target, *Result) {
	target, err := t.hooks.Resolve(ctx, raw)
	if err != nil {
		var verbatim verbatimError
		if errors.As(err, &verbatim) {
			bad := failure("%s", verbatim.text)
			return Target{}, &bad
		}
		bad := failure("invalid URL: %v", err)
		return Target{}, &bad
	}
	return target, nil
}

func (t *Tools) readToken(ctx context.Context, host string) string {
	if t.hooks.ReadToken == nil {
		return ""
	}
	return t.hooks.ReadToken(ctx, host)
}

// fetch reads path on at's server with the read token for that host.
func (t *Tools) fetch(ctx context.Context, at Target, path string) (fetch.Result, error) {
	return t.backend.Fetch(ctx, fetch.FetchRequest{Host: at.Host, Path: path, Token: t.readToken(ctx, at.Host)})
}

// failed words a backend error. A write that was sent may have landed, so its
// failure says so: a blind resend conflicts with the first attempt.
func (t *Tools) failed(site Site, host string, err error) Result {
	msg := fmt.Sprintf("%s failed: %v", site, err)
	if site == SiteResolveIndex {
		msg = fmt.Sprintf("failed to fetch index: %v", err)
	}
	if t.hooks.ErrText != nil {
		msg = t.hooks.ErrText(site, host, err)
	}
	if errors.Is(err, fetch.ErrOutcomeUnknown) {
		msg += "; it may have landed: fetch the document before retrying"
	}
	return Result{Text: msg, IsError: true}
}

// writeFailed is failed for a write: a version that could not be resolved is
// the contract's own refusal and reads as it is worded there.
func (t *Tools) writeFailed(site Site, host string, err error) Result {
	var unresolved *docwrite.VersionError
	if errors.As(err, &unresolved) {
		return failure("%v", unresolved)
	}
	return t.failed(site, host, err)
}

func (t *Tools) warnf(format string, args ...any) {
	if t.hooks.Warnf != nil {
		t.hooks.Warnf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Verbatim is a hook error whose text is the tool's whole answer, with no
// prefix added: a surface's own wording for its own refusals.
func Verbatim(text string) error { return verbatimError{text: text} }

type verbatimError struct{ text string }

func (e verbatimError) Error() string { return e.text }
