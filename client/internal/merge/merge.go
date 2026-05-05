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

// Client is the subset of fetch.Client operations MergeAndPublish needs.
// Defined here so the merge package is testable without QUIC.
type Client interface {
	FetchVersion(path string, version int) (Doc, error)
	FetchCurrent(path string) (Doc, error)
	Publish(path, body string, expectedVersion int, meta map[string]string) (PublishResult, error)
}

// OutcomeStatus describes the final state of a MergeAndPublish call.
type OutcomeStatus string

const (
	// OutcomeOK means the publish succeeded. Merged is true if a diff3
	// merge happened along the way.
	OutcomeOK OutcomeStatus = "ok"
	// OutcomeConflict means diff3 produced conflict markers and the
	// caller's LLM must resolve them. ConflictBody holds the marked merge.
	OutcomeConflict OutcomeStatus = "conflict"
	// OutcomeContention means the merge loop exhausted maxRetries against
	// a path that kept advancing. The caller may retry.
	OutcomeContention OutcomeStatus = "contention"
)

// Outcome is the result of a MergeAndPublish call.
type Outcome struct {
	Status       OutcomeStatus
	Publish      PublishResult // populated when Status == OutcomeOK
	Merged       bool          // true when at least one diff3 merge happened
	BaseVersion  int           // the version we merged from (Status != OutcomeOK or Merged)
	TheirVersion int           // the latest version we merged against
	ConflictBody string        // diff3 output with markers (Status == OutcomeConflict)
	Retries      int           // number of merge attempts (0 if first publish succeeded)
}

// ErrInvalidExpectedVersion is returned when expectedVersion is < 0.
var ErrInvalidExpectedVersion = errors.New("expected_version must be >= 0")

// statusConflict matches protocol.StatusConflict. Compared as a raw string
// so this package does not depend on the protocol package.
const statusConflict = "conflict"

// statusOK matches protocol.StatusOK for fetch responses.
const statusOK = "ok"

// MergeAndPublish publishes body to path with optimistic concurrency. On
// version conflict, it fetches the base version (the one the agent edited
// from) and the current version, runs diff3, and republishes the merged
// body. It retries up to maxRetries times when the republish itself
// conflicts (a third writer slipped in). Returns OutcomeConflict if diff3
// produces markers; OutcomeContention when retries are exhausted.
//
// expectedVersion = 0 means create-only, identical to fetch.Client.Publish.
// In that mode, conflicts are not eligible for diff3 merge — there is no
// base to merge from — and the conflict is returned as-is.
//
//nolint:revive // MergeAndPublish reads more clearly at call sites than merge.AndPublish or merge.Publish.
func MergeAndPublish(c Client, path, body string, expectedVersion, maxRetries int, meta map[string]string) (Outcome, error) {
	if expectedVersion < 0 {
		return Outcome{}, ErrInvalidExpectedVersion
	}

	first, err := c.Publish(path, body, expectedVersion, meta)
	if err != nil {
		return Outcome{}, fmt.Errorf("initial publish: %w", err)
	}
	if first.Status != statusConflict {
		return Outcome{Status: OutcomeOK, Publish: first}, nil
	}
	// expectedVersion == 0 means "create only". A conflict here means the
	// document already exists; there is no base to merge from.
	if expectedVersion == 0 {
		return Outcome{
			Status:       OutcomeConflict,
			TheirVersion: first.ServerVersion,
		}, nil
	}

	base, err := c.FetchVersion(path, expectedVersion)
	if err != nil {
		return Outcome{}, fmt.Errorf("fetch base v%d: %w", expectedVersion, err)
	}
	if base.Status != statusOK {
		return Outcome{}, fmt.Errorf("fetch base v%d: status %s", expectedVersion, base.Status)
	}

	// mergeBase advances forward with each retry: after a successful diff3
	// the merged body is "based on" whatever latest we just merged with,
	// so the next iteration uses that as base. Otherwise we'd re-conflict
	// on the prior iteration's already-resolved changes.
	mergeBase := base.Body
	mergeOurs := body
	merged := false
	lastLatestVersion := 0

	for attempt := range maxRetries {
		latest, err := c.FetchCurrent(path)
		if err != nil {
			return Outcome{}, fmt.Errorf("fetch latest: %w", err)
		}
		if latest.Status != statusOK {
			return Outcome{}, fmt.Errorf("fetch latest: status %s", latest.Status)
		}
		lastLatestVersion = latest.Version

		mergeRes := Diff3(mergeBase, mergeOurs, latest.Body)
		merged = true
		if mergeRes.Conflict {
			return Outcome{
				Status:       OutcomeConflict,
				Merged:       true,
				BaseVersion:  expectedVersion,
				TheirVersion: latest.Version,
				ConflictBody: mergeRes.Body,
				Retries:      attempt,
			}, nil
		}

		next, err := c.Publish(path, mergeRes.Body, latest.Version, meta)
		if err != nil {
			return Outcome{}, fmt.Errorf("publish merged (attempt %d): %w", attempt+1, err)
		}
		if next.Status != statusConflict {
			return Outcome{
				Status:       OutcomeOK,
				Publish:      next,
				Merged:       merged,
				BaseVersion:  expectedVersion,
				TheirVersion: latest.Version,
				Retries:      attempt + 1,
			}, nil
		}

		mergeBase = latest.Body
		mergeOurs = mergeRes.Body
	}

	return Outcome{
		Status:       OutcomeContention,
		Merged:       merged,
		BaseVersion:  expectedVersion,
		TheirVersion: lastLatestVersion,
		Retries:      maxRetries,
	}, nil
}
