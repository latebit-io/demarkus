package merge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Doc is a fetched document used as input to a merge.
type Doc struct {
	Status   string
	Body     string
	Version  int
	Metadata map[string]string // response metadata, publisher keys included
}

// PublishResult is what a publish returns. Status is the raw protocol status
// ("ok", "created", "conflict", etc). On a version conflict, ServerVersion
// holds the server's current version; otherwise it is zero.
type PublishResult struct {
	Status        string
	Version       int
	ServerVersion int
	Metadata      map[string]string
	// Body is the server's own words: on a refusal, what to fix.
	Body string
}

// on_conflict values a publish accepts.
const (
	OnConflictMerge = "merge"
	OnConflictFail  = "fail"
)

// ParseOnConflict normalizes a tool's on_conflict argument: blank means
// merge, which MCP clients commonly send for an omitted optional field.
func ParseOnConflict(raw string) (string, error) {
	mode := strings.TrimSpace(raw)
	if mode == "" {
		return OnConflictMerge, nil
	}
	if mode != OnConflictMerge && mode != OnConflictFail {
		return "", fmt.Errorf("invalid on_conflict %q: expected \"merge\" or \"fail\"", mode)
	}
	return mode, nil
}

// Client is the subset of fetch.Client operations Candidate needs.
// Defined here so the merge package is testable without QUIC.
type Client interface {
	FetchVersion(ctx context.Context, path string, version int) (Doc, error)
	FetchCurrent(ctx context.Context, path string) (Doc, error)
	Publish(ctx context.Context, w Write) (PublishResult, error)
}

// Write is one publish attempt. ExpectedVersion 0 creates only; a conflict in
// that mode merges against an empty base.
type Write struct {
	Path, Body      string
	ExpectedVersion int
	Metadata        map[string]string
}

// OutcomeStatus describes the result of a Candidate call.
type OutcomeStatus string

const (
	// OutcomeOK means the initial publish succeeded — no merge was needed.
	OutcomeOK OutcomeStatus = "ok"
	// OutcomeCandidate means the publish conflicted and the tool produced a
	// diff3 merge candidate. The agent should semantically review the body
	// (resolving any markers) and republish at PublishAtVersion.
	OutcomeCandidate OutcomeStatus = "merge-candidate"
)

// Outcome is the result of a Candidate call. Fields are populated based
// on Status — Publish on OutcomeOK; Body, HasMarkers, BaseVersion,
// TheirVersion, and PublishAtVersion on OutcomeCandidate.
type Outcome struct {
	Status           OutcomeStatus
	Publish          PublishResult
	Body             string
	HasMarkers       bool
	BaseVersion      int
	TheirVersion     int
	PublishAtVersion int
}

// ErrInvalidExpectedVersion is returned when expectedVersion is < 0.
var ErrInvalidExpectedVersion = errors.New("expected_version must be >= 0")

// statusConflict matches protocol.StatusConflict. Compared as a raw string
// so this package does not depend on the protocol package.
const statusConflict = "conflict"

// statusOK matches protocol.StatusOK for fetch responses.
const statusOK = "ok"

// statusNotFound matches protocol.StatusNotFound.
const statusNotFound = "not-found"

// Landed reports whether the head is exactly what w submitted: body at
// ExpectedVersion+1 with every submitted metadata key. A failed probe reads as
// not landed; the caller's error stands.
func Landed(ctx context.Context, c Client, w Write) (Doc, bool) {
	head, err := c.FetchCurrent(ctx, w.Path)
	if err != nil || head.Status != statusOK {
		return Doc{}, false
	}
	return head, w.matches(head)
}

// matches compares version, body and every submitted metadata key, so another
// writer's identical body with different metadata is not mistaken for ours.
func (w Write) matches(head Doc) bool {
	if head.Version != w.ExpectedVersion+1 || head.Body != w.Body {
		return false
	}
	for k, v := range w.Metadata {
		// An absent key is a mismatch even when the submitted value is empty.
		actual, ok := head.Metadata[k]
		if !ok || strings.TrimSpace(actual) != strings.TrimSpace(v) {
			return false
		}
	}
	return true
}

// Candidate publishes w. On a version conflict it returns a diff3 candidate
// (base: the version edited from, theirs: the head) and never publishes it:
// disjoint lines are not disjoint meaning, so the caller reviews and republishes.
func Candidate(ctx context.Context, c Client, w Write) (Outcome, error) {
	if w.ExpectedVersion < 0 {
		return Outcome{}, ErrInvalidExpectedVersion
	}

	pub, err := c.Publish(ctx, w)
	if err != nil {
		// The write may have landed with its response lost; never resend it.
		if head, ok := Landed(ctx, c, w); ok {
			return Outcome{Status: OutcomeOK, Publish: Reconciled(head)}, nil
		}
		return Outcome{}, fmt.Errorf("publish: %w", err)
	}
	if pub.Status != statusConflict {
		return Outcome{Status: OutcomeOK, Publish: pub}, nil
	}

	latest, err := c.FetchCurrent(ctx, w.Path)
	if err != nil {
		return Outcome{}, fmt.Errorf("fetch current: %w", err)
	}
	if latest.Status != statusOK {
		return Outcome{}, fmt.Errorf("fetch current: status %s", latest.Status)
	}
	// A conflict against our own earlier attempt is a success, not a merge.
	if w.matches(latest) {
		return Outcome{Status: OutcomeOK, Publish: Reconciled(latest)}, nil
	}

	base := ""
	if w.ExpectedVersion > 0 {
		baseDoc, err := c.FetchVersion(ctx, w.Path, w.ExpectedVersion)
		if err != nil {
			return Outcome{}, fmt.Errorf("fetch base v%d: %w", w.ExpectedVersion, err)
		}
		// No base: it never existed, or retention pruned it. Nothing to merge
		// against, so the server's conflict is the answer.
		if baseDoc.Status == statusNotFound {
			return Outcome{Status: OutcomeOK, Publish: pub}, nil
		}
		if baseDoc.Status != statusOK {
			return Outcome{}, fmt.Errorf("fetch base v%d: status %s", w.ExpectedVersion, baseDoc.Status)
		}
		base = baseDoc.Body
	}

	// Without a real head version the follow-up publish would fall into
	// create-only semantics (expected_version=0); fail fast instead.
	if latest.Version <= 0 {
		return Outcome{}, fmt.Errorf("fetch current: missing or invalid version metadata")
	}

	merged := Diff3(base, w.Body, latest.Body)
	return Outcome{
		Status:           OutcomeCandidate,
		Body:             merged.Body,
		HasMarkers:       merged.Conflict,
		BaseVersion:      w.ExpectedVersion,
		TheirVersion:     latest.Version,
		PublishAtVersion: latest.Version,
	}, nil
}

// Reconciled is the result of a write found at the head instead of answered:
// ok at the head's version, which is all that is known about it.
func Reconciled(head Doc) PublishResult {
	return PublishResult{Status: statusOK, Version: head.Version, Metadata: map[string]string{"version": strconv.Itoa(head.Version)}}
}
