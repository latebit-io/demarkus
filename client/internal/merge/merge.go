package merge

import (
	"errors"
	"fmt"
)

// Doc is a fetched document used as input to a merge.
type Doc struct {
	Status  string
	Body    string
	Version int
}

// PublishResult is what a publish returns. Status is the raw protocol status
// ("ok", "created", "conflict", etc). On a version conflict, ServerVersion
// holds the server's current version; otherwise it is zero.
type PublishResult struct {
	Status        string
	Version       int
	ServerVersion int
	Metadata      map[string]string
}

// Client is the subset of fetch.Client operations Candidate needs.
// Defined here so the merge package is testable without QUIC.
type Client interface {
	FetchVersion(path string, version int) (Doc, error)
	FetchCurrent(path string) (Doc, error)
	Publish(path, body string, expectedVersion int, meta map[string]string) (PublishResult, error)
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

// Candidate publishes body to path with optimistic concurrency. On a
// version mismatch it produces a diff3 merge candidate (base = the version
// the agent edited from, theirs = the current latest, ours = body) and
// returns it for the agent to semantically verify and republish. The tool
// never auto-publishes the merge — line-level disjoint ≠ semantically
// disjoint, so the agent must always inspect the candidate before it
// becomes the new head.
//
// Iteration is the agent's responsibility: if the agent's follow-up publish
// itself conflicts (a third writer slipped in), the agent simply calls
// Candidate again with the new body and version.
//
// expectedVersion = 0 means create-only. Conflicts in that mode merge
// against an empty base — non-overlapping insertions on both sides make it
// through cleanly; overlapping insertions get markers.
func Candidate(c Client, path, body string, expectedVersion int, meta map[string]string) (Outcome, error) {
	if expectedVersion < 0 {
		return Outcome{}, ErrInvalidExpectedVersion
	}

	pub, err := c.Publish(path, body, expectedVersion, meta)
	if err != nil {
		return Outcome{}, fmt.Errorf("publish: %w", err)
	}
	if pub.Status != statusConflict {
		return Outcome{Status: OutcomeOK, Publish: pub}, nil
	}

	base := ""
	if expectedVersion > 0 {
		baseDoc, err := c.FetchVersion(path, expectedVersion)
		if err != nil {
			return Outcome{}, fmt.Errorf("fetch base v%d: %w", expectedVersion, err)
		}
		if baseDoc.Status != statusOK {
			return Outcome{}, fmt.Errorf("fetch base v%d: status %s", expectedVersion, baseDoc.Status)
		}
		base = baseDoc.Body
	}

	latest, err := c.FetchCurrent(path)
	if err != nil {
		return Outcome{}, fmt.Errorf("fetch current: %w", err)
	}
	if latest.Status != statusOK {
		return Outcome{}, fmt.Errorf("fetch current: status %s", latest.Status)
	}
	// A candidate without a real head version would force the agent's
	// follow-up publish into create-only semantics (expected_version=0),
	// which would either silently change behavior or fail at the server.
	// Fail fast instead of returning an unusable PublishAtVersion.
	if latest.Version <= 0 {
		return Outcome{}, fmt.Errorf("fetch current: missing or invalid version metadata")
	}

	merged := Diff3(base, body, latest.Body)
	return Outcome{
		Status:           OutcomeCandidate,
		Body:             merged.Body,
		HasMarkers:       merged.Conflict,
		BaseVersion:      expectedVersion,
		TheirVersion:     latest.Version,
		PublishAtVersion: latest.Version,
	}, nil
}
