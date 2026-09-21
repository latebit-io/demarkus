package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/client/docwrite"
	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/merge"
	"github.com/latebit-io/demarkus/protocol"
)

// cliWrite is one PUBLISH, APPEND or ARCHIVE from the command line.
type cliWrite struct {
	verb            string
	doc             *docwrite.Doc
	body            string
	expectedVersion *int // nil when -expected-version was not given
	force           bool
	onConflict      string
	meta            map[string]string
}

// writeOutcome is what the command prints and how it exits. notice goes to
// stderr, stdout to stdout; code is the process exit code.
type writeOutcome struct {
	status   string
	metadata map[string]string
	stdout   string
	notice   string
	code     int
}

// runWrite applies the write contract every surface shares. The error is a
// refused argument or a failed request; a refusing status is an outcome.
func runWrite(ctx context.Context, w *cliWrite) (writeOutcome, error) {
	switch w.verb {
	case protocol.VerbPublish:
		return publishFromCLI(ctx, w)
	case protocol.VerbAppend:
		if w.body == "" {
			return writeOutcome{}, errors.New("APPEND requires a body: use -body or pipe content via stdin")
		}
		version := 0 // zero resolves the current version
		if w.expectedVersion != nil {
			version = *w.expectedVersion
		}
		result, err := w.doc.Append(ctx, docwrite.AppendRequest{Body: w.body, ExpectedVersion: version, Metadata: w.meta})
		return answeredOutcome(result, err)
	case protocol.VerbArchive:
		result, err := w.doc.Archive(ctx)
		return answeredOutcome(result, err)
	}
	return writeOutcome{}, fmt.Errorf("%s is not a write", w.verb)
}

func publishFromCLI(ctx context.Context, w *cliWrite) (writeOutcome, error) {
	if w.force {
		result, err := w.doc.PublishUnchecked(ctx, w.body, w.meta)
		return answeredOutcome(result, err)
	}
	if w.expectedVersion == nil || *w.expectedVersion < 0 {
		return writeOutcome{}, errors.New("PUBLISH requires -expected-version: 0 to create, N to replace version N; " +
			"-force overwrites whatever is there")
	}
	mode, err := merge.ParseOnConflict(w.onConflict)
	if err != nil {
		return writeOutcome{}, err
	}
	outcome, err := w.doc.Publish(ctx, merge.Write{Path: w.doc.Path, Body: w.body, ExpectedVersion: *w.expectedVersion, Metadata: w.meta}, mode)
	if err != nil {
		return writeOutcome{}, unknownOutcomeAdvice(err)
	}
	if outcome.Status == merge.OutcomeCandidate {
		return candidateOutcome(&outcome), nil
	}
	return responseOutcome(outcome.Publish.Status, outcome.Publish.Metadata, outcome.Publish.Body), nil
}

// candidateOutcome hands the merged body to stdout so it can be redirected,
// and the facts needed to republish it to stderr.
func candidateOutcome(o *merge.Outcome) writeOutcome {
	notice := fmt.Sprintf("[merge-candidate] your-version=%d current-version=%d publish-at-version=%d has-markers=%t\n"+
		"demarkus: conflict; the merged candidate is on stdout: review it, then publish it with -expected-version %d\n",
		o.BaseVersion, o.TheirVersion, o.PublishAtVersion, o.HasMarkers, o.PublishAtVersion)
	return writeOutcome{status: string(merge.OutcomeCandidate), stdout: o.Body, notice: notice, code: 1}
}

func answeredOutcome(result docwrite.Result, err error) (writeOutcome, error) { //nolint:gocritic // takes the call's two results as they come
	if err != nil {
		return writeOutcome{}, unknownOutcomeAdvice(err)
	}
	if result.Reconciled {
		result.Response.Metadata["reconciled"] = "true"
	}
	return responseOutcome(result.Response.Status, result.Response.Metadata, result.Response.Body), nil
}

// responseOutcome is a write the server answered: its exit code is its status.
func responseOutcome(status string, meta map[string]string, body string) writeOutcome {
	return writeOutcome{status: status, metadata: meta, stdout: body, code: exitCodeForStatus(status)}
}

// unknownOutcomeAdvice keeps a lost response from reading like a plain
// failure: resending a write that landed conflicts with itself.
func unknownOutcomeAdvice(err error) error {
	if errors.Is(err, fetch.ErrOutcomeUnknown) {
		return fmt.Errorf("%w; the write may have landed: fetch the document before retrying", err)
	}
	return err
}

// print writes the outcome the way every request prints: the status line on
// stderr when verbose or when it is a notice, the body on stdout.
func (o *writeOutcome) print(verbose bool) {
	if o.notice != "" {
		fmt.Fprint(os.Stderr, o.notice)
	} else if verbose {
		fmt.Fprintln(os.Stderr, statusLine(o.status, o.metadata))
	}
	fmt.Print(o.stdout)
	if o.code != 0 && o.notice == "" {
		fmt.Fprintf(os.Stderr, "demarkus: %s\n", o.status)
	}
}

// statusLine is "[status] k=v ..." with the keys in a stable order.
func statusLine(status string, meta map[string]string) string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]", status)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%s", k, meta[k])
	}
	return b.String()
}

// editedDoc is what came back from the editor.
type editedDoc struct {
	body           string
	fetchedVersion int // 0 when the document did not exist
	meta           map[string]string
}

// finishEdit publishes the edited body. Whatever goes wrong, the work is on
// disk and the notice says where: a merged candidate on a conflict, the edits
// themselves on any other failure.
func finishEdit(ctx context.Context, doc *docwrite.Doc, edited editedDoc) writeOutcome { //nolint:gocritic // one edit, passed once
	outcome, err := doc.Publish(ctx, merge.Write{Path: doc.Path, Body: edited.body, ExpectedVersion: edited.fetchedVersion, Metadata: edited.meta}, merge.OnConflictMerge)
	if err != nil {
		return savedOutcome(doc.Path, edited.body, fmt.Sprintf("publish failed: %v", unknownOutcomeAdvice(err)))
	}
	if outcome.Status == merge.OutcomeCandidate {
		markers := "it merged cleanly"
		if outcome.HasMarkers {
			markers = "it holds conflict markers to resolve"
		}
		return savedOutcome(doc.Path, outcome.Body, fmt.Sprintf(
			"Conflict: the document is at version %d, you edited version %d.\nA merged candidate was written; %s.\n"+
				"Review it, then publish it with -expected-version %d.",
			outcome.TheirVersion, outcome.BaseVersion, markers, outcome.PublishAtVersion))
	}
	return responseOutcome(outcome.Publish.Status, outcome.Publish.Metadata, outcome.Publish.Body)
}

// savedOutcome writes body to a fresh temp file and reports why and where.
func savedOutcome(docPath, body, why string) writeOutcome {
	file, err := saveWork(docPath, body)
	if err != nil {
		return writeOutcome{notice: fmt.Sprintf("%s\nfailed to save your work: %v\n", why, err), code: 1}
	}
	return writeOutcome{notice: fmt.Sprintf("%s\nYour work was saved to %s\n", why, file), code: 1}
}

// saveWork uses CreateTemp, so a predictable name cannot be a planted symlink.
func saveWork(docPath, body string) (string, error) {
	safeName := strings.ReplaceAll(strings.TrimLeft(docPath, "/"), "/", "-")
	f, err := os.CreateTemp("", "demarkus-edit-"+safeName+"-*.md")
	if err != nil {
		return "", err
	}
	// A failed close can lose buffered bytes, so it counts as a failed save.
	_, writeErr := f.WriteString(body)
	if saveErr := errors.Join(writeErr, f.Close()); saveErr != nil {
		return "", saveErr
	}
	return f.Name(), nil
}

// editedFrom pairs the edited body with what the fetch knew: the version to
// check against and the publisher metadata to send back, since a PUBLISH
// replaces the metadata map and an edit must not strip a document's tags.
func editedFrom(fetched protocol.Response, newBody string) (editedDoc, error) { //nolint:gocritic // a response, read once
	switch fetched.Status {
	case protocol.StatusNotFound:
		return editedDoc{body: newBody}, nil // version 0: create only
	case protocol.StatusOK:
	default:
		return editedDoc{}, fmt.Errorf("fetch failed: %s", fetched.Status)
	}
	version, err := strconv.Atoi(fetched.Metadata["version"])
	if err != nil || version < 1 {
		return editedDoc{}, fmt.Errorf("document has no usable version (%q), so an edit cannot be checked against it", fetched.Metadata["version"])
	}
	return editedDoc{body: newBody, fetchedVersion: version, meta: publisherMeta(fetched.Metadata)}, nil
}

// directDoc binds the write contract to a document reached directly: one
// token for reads and writes, and a write is sent once.
func directDoc(client docwrite.Backend, host, path, token string) *docwrite.Doc {
	return &docwrite.Doc{Backend: client, Host: host, Path: path, ReadToken: token, Write: docwrite.SendOnce(token)}
}

func isWriteVerb(verb string) bool {
	return verb == protocol.VerbPublish || verb == protocol.VerbAppend || verb == protocol.VerbArchive
}

// givenInt is the flag's value, nil when it was left at its default.
func givenInt(name string, value *int) *int {
	var given *int
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = value
		}
	})
	return given
}

// saveEdit publishes what came back from the editor and returns the exit
// code. It runs under its own interrupt context: a Ctrl-C typed into the
// editor must not cancel the save.
func saveEdit(doc *docwrite.Doc, fetched protocol.Response, newBody string) int { //nolint:gocritic // a response, read once
	work, err := editedFrom(fetched, newBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := interruptContext()
	defer stop()
	outcome := finishEdit(ctx, doc, work)
	outcome.print(true)
	return outcome.code
}
