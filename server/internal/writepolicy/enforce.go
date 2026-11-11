package writepolicy

import (
	"context"
	"fmt"
	"slices"

	"github.com/latebit-io/demarkus/protocol/contentcheck"
	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"github.com/latebit-io/demarkus/protocol/storefmt"
	"github.com/latebit-io/demarkus/server/internal/backend"
)

// Options configures enforcement.
type Options struct {
	// Require makes a missing policy document a refusal instead of the
	// empty policy.
	Require bool
	// Floor reads the floor world's policy, merged under this world's so
	// the world can only tighten it. Nil means no floor. An error refuses
	// the write: a floor that cannot be read is not skipped.
	Floor func(ctx context.Context) (publishpolicy.Policy, error)
	// Worlds lists the policies of every world under this one's floor; set
	// on the floor world so a candidate floor is refused when it would
	// leave a world unsatisfiable. Nil skips the check.
	Worlds func(ctx context.Context) ([]publishpolicy.Policy, error)
	// Content is the secrets and personal-data scan.
	Content ContentOptions
	// Classify is the classifier stamp for a written body ("" for none),
	// set as protocol.ClassifierKey before the commit; nil classifies nothing.
	Classify func(ctx context.Context, path, body string) string
}

// Enforce wraps store so every PUBLISH and APPEND is judged by the world's
// policy document. Reads pass through untouched.
func Enforce(store backend.Store, options Options) *Enforcer {
	// The near-duplicate gate judges against the section index; a store
	// that builds it on demand starts it now, ahead of the first write.
	if warm, ok := store.(backend.SectionWarmer); ok {
		warm.WarmSections()
	}
	return &Enforcer{Store: store, current: CurrentOf(store), options: options, content: newContentGate(options.Content)}
}

// Enforcer is a store behind the write gate: a backend.NotingStore, and a
// backend.CurrentReader like the store it wraps.
type Enforcer struct {
	backend.Store
	current backend.CurrentReader
	options Options
	content contentGate
}

var (
	_ backend.NotingStore   = (*Enforcer)(nil)
	_ backend.CurrentReader = (*Enforcer)(nil)
)

// ReadCurrent reads through the wrapped store; no gate judges a read.
func (e *Enforcer) ReadCurrent(ctx context.Context, path string) (*storefmt.Document, error) {
	return e.current.ReadCurrent(ctx, path)
}

// chain runs the caller's precondition, then the enforcer's.
func chain[S, T any](inner, next func(context.Context, S, T) error) func(context.Context, S, T) error {
	if inner == nil {
		return next
	}
	return func(ctx context.Context, state S, change T) error {
		if err := inner(ctx, state, change); err != nil {
			return err
		}
		return next(ctx, state, change)
	}
}

// guard judges near-duplicates and classifies before the commit, the
// near-duplicates again inside it, then the policy. It returns where the
// commit leaves the notes for the sender; a retried commit replaces them.
func (e *Enforcer) guard(ctx context.Context, req *backend.WriteRequest, sender backend.Sender, appending bool) (*[]string, error) {
	if sender.Readable == nil {
		sender.Readable = func(string) bool { return true }
	}
	before, dependents, err := e.beforeCommit(ctx, *req, sender)
	if err != nil {
		return nil, err
	}
	if _, err := e.settleNearDuplicate(before); err != nil {
		return nil, err
	}
	lead := e.classify(ctx, req, appending)
	d := drift{readable: sender.Readable, dependents: dependents}
	noted := new([]string)
	if e.options.Content.NearDuplicate == contentcheck.Off {
		req.Precondition = chain(req.Precondition, e.policyOnly(noted, lead, d))
		return noted, nil
	}
	subject := subjectOf(*req)
	req.Precondition = chain(req.Precondition, func(ctx context.Context, state backend.CommitState, write storefmt.PreparedWrite) error {
		notes, err := e.evaluate(ctx, state, write, d)
		*noted = slices.Concat(lead, notes)
		if err != nil {
			return err
		}
		// A commit state with no section index (ErrBodyMatchUnavailable) leaves
		// the verdict read before the commit standing: that store's race.
		verdict := before
		again, err := e.judgeNearDuplicate(ctx, state, subject)
		if again, err = e.tolerate(again, err); err != nil {
			return err
		}
		if again.checked {
			verdict = again
		}
		near, err := e.settleNearDuplicate(verdict)
		*noted = slices.Concat(lead, notes, near)
		return err
	})
	return noted, nil
}

// tolerate decides a judgment that could not run: block mode refuses the
// write rather than let an unchecked body land; warn mode notes it.
func (e *Enforcer) tolerate(verdict nearVerdict, err error) (nearVerdict, error) {
	if err == nil || e.options.Content.NearDuplicate != contentcheck.Warn {
		return verdict, err
	}
	return nearVerdict{notes: []string{"near-duplicate check skipped: " + err.Error()}}, nil
}

// policyOnly is the precondition with the near-duplicate check off.
func (e *Enforcer) policyOnly(noted *[]string, lead []string, d drift) backend.Precondition {
	return func(ctx context.Context, state backend.CommitState, write storefmt.PreparedWrite) error {
		notes, err := e.evaluate(ctx, state, write, d)
		*noted = slices.Concat(lead, notes)
		return err
	}
}

// Publish is PublishNoted for a sender who reads everything, notes dropped.
func (e *Enforcer) Publish(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	doc, _, err := e.PublishNoted(ctx, req, backend.Sender{})
	return doc, err
}

// PublishNoted judges the write, then commits it; the notes are what the
// gate let through, and only a write that landed has any.
func (e *Enforcer) PublishNoted(ctx context.Context, req backend.WriteRequest, sender backend.Sender) (*storefmt.Document, []string, error) {
	noted, err := e.guard(ctx, &req, sender, false)
	if err != nil {
		return nil, nil, err
	}
	return landed(noted)(e.Store.Publish(ctx, req))
}

// Append is AppendNoted for a sender who reads everything, notes dropped.
func (e *Enforcer) Append(ctx context.Context, req backend.WriteRequest) (*storefmt.Document, error) {
	doc, _, err := e.AppendNoted(ctx, req, backend.Sender{})
	return doc, err
}

// AppendNoted judges the fragment, not the merged body: the target's own
// text is what the fragment would repeat elsewhere, never a duplicate of itself.
func (e *Enforcer) AppendNoted(ctx context.Context, req backend.WriteRequest, sender backend.Sender) (*storefmt.Document, []string, error) {
	noted, err := e.guard(ctx, &req, sender, true)
	if err != nil {
		return nil, nil, err
	}
	return landed(noted)(e.Store.Append(ctx, req))
}

// landed pairs a commit's outcome with its notes. A commit that failed may
// still be running its precondition (an outcome unknown): its notes are not read.
func landed(noted *[]string) func(*storefmt.Document, error) (*storefmt.Document, []string, error) {
	return func(doc *storefmt.Document, err error) (*storefmt.Document, []string, error) {
		if err != nil {
			return doc, nil, err
		}
		return doc, *noted, nil
	}
}

// currentReader adapts a lock-free current read to the one Get Current asks.
type currentReader struct{ store backend.CurrentReader }

func (r currentReader) Get(ctx context.Context, path string, _ int) (*storefmt.Document, error) {
	return r.store.ReadCurrent(ctx, path)
}

// SetArchived refuses to archive a required policy: the world would then
// refuse every write. The guard runs inside the commit, after the caller's own.
func (e *Enforcer) SetArchived(ctx context.Context, req backend.ArchiveRequest) (backend.ArchiveResult, error) {
	req.Precondition = chain(req.Precondition, e.keepRequiredPolicy)
	return e.Store.SetArchived(ctx, req)
}

func (e *Enforcer) keepRequiredPolicy(_ context.Context, _ backend.Reader, change storefmt.ArchiveChange) error {
	if change.Archived && e.options.Require && change.Path == publishpolicy.DocumentPath {
		return fmt.Errorf("%w: %w: required policy cannot be archived", backend.ErrRejected, ErrInvalidPolicy)
	}
	return nil
}

// Validate reports whether the world holds a usable policy document, which a
// server checks before it starts enforcing one.
func Validate(ctx context.Context, store backend.Store) error {
	return inspect(ctx, store)
}

// inspect reads the current policy and returns its verdict.
func inspect(ctx context.Context, store backend.Store) error {
	_, err := Read(ctx, CurrentOf(store), true)
	return err
}

// Read is the world's current policy, read outside any view: it runs inside
// another world's commit, where a view's read lock against that world's own
// commit would be the second half of a deadlock.
func Read(ctx context.Context, store backend.CurrentReader, require bool) (publishpolicy.Policy, error) {
	return Current(ctx, currentReader{store}, require)
}

// CurrentOf is how one current document of store is read outside a view: the
// store's own lock-free read (backend.CurrentReader), else a view opened for
// the one read, which must then not run inside a commit of the same store.
func CurrentOf(store backend.Store) backend.CurrentReader {
	if current, ok := store.(backend.CurrentReader); ok {
		return current
	}
	return viewCurrent{store}
}

// viewCurrent reads a current document through a view of its own.
type viewCurrent struct{ store backend.ViewProvider }

func (v viewCurrent) ReadCurrent(ctx context.Context, path string) (document *storefmt.Document, err error) {
	view, err := v.store.OpenReadView(ctx)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() {
		if closeErr := view.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("read %s: close view: %w", path, closeErr)
		}
	}()
	return view.Get(ctx, path, 0)
}
