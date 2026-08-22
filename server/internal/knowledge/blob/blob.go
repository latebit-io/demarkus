// Package blob defines generation-aware object storage for knowledge buckets.
package blob

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const (
	// MaxKeyBytes is the maximum encoded object-key length.
	MaxKeyBytes = 1024
	// MaxListPage is the fixed number of objects returned by List.
	MaxListPage = 1000
	// MaxCursorPayloadBytes bounds a driver's opaque continuation payload.
	MaxCursorPayloadBytes = 2048

	cursorVersion          = byte(1)
	cursorHeaderBytes      = 3
	maxCursorEnvelopeBytes = cursorHeaderBytes + MaxKeyBytes + MaxCursorPayloadBytes
	maxEncodedCursorBytes  = 4 * ((maxCursorEnvelopeBytes + 2) / 3)
)

// Generation identifies one object revision. Values support equality only;
// ordering is provider-specific.
type Generation int64

// Attributes describe one object revision.
type Attributes struct {
	Key        string
	Generation Generation
	Size       int64
	Modified   time.Time // UTC, second precision; used only for object lifecycle.
}

// Object contains object bytes and their attributes.
type Object struct {
	Data       []byte
	Attributes Attributes
}

// ListResult contains one lexicographic page and its continuation cursor.
type ListResult struct {
	Objects    []Attributes
	NextCursor string
}

// Store is a generation-aware object store.
type Store interface {
	Get(context.Context, string) (Object, error)
	Head(context.Context, string) (Attributes, error)
	Create(context.Context, string, []byte) (Attributes, error)
	Replace(context.Context, string, Generation, []byte) (Attributes, error)
	Delete(context.Context, string, Generation) error
	List(context.Context, string, string) (ListResult, error)
}

var (
	// ErrNotFound reports a missing object on a read.
	ErrNotFound = errors.New("blob not found")
	// ErrPrecondition reports invalid input or a failed generation condition.
	ErrPrecondition = errors.New("blob precondition failed")
	// ErrThrottled reports temporary service rate limiting.
	ErrThrottled = errors.New("blob throttled")
	// ErrUnavailable reports a temporary service failure.
	ErrUnavailable = errors.New("blob unavailable")
	// ErrIntegrity reports corrupt or inconsistent object state.
	ErrIntegrity = errors.New("blob integrity failure")
	// ErrAmbiguous reports an operation whose outcome cannot be determined.
	ErrAmbiguous = errors.New("blob outcome ambiguous")
)

// OpError adds operation and key context to a blob error.
type OpError struct {
	Op  string
	Key string
	Err error
}

// Error implements error.
func (e *OpError) Error() string {
	if e.Key == "" {
		return fmt.Sprintf("blob %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("blob %s %q: %v", e.Op, e.Key, e.Err)
}

// Unwrap returns the underlying error category or context error.
func (e *OpError) Unwrap() error {
	return e.Err
}

// ValidateKey checks the shared object-key contract.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrPrecondition)
	}
	if key == "." || key == ".." {
		return fmt.Errorf("%w: key %q is reserved", ErrPrecondition, key)
	}
	return validateText("key", key)
}

// ValidatePrefix checks the shared list-prefix contract.
func ValidatePrefix(prefix string) error {
	return validateText("prefix", prefix)
}

// ValidateGeneration rejects non-positive generation conditions.
func ValidateGeneration(generation Generation) error {
	if generation <= 0 {
		return fmt.Errorf("%w: generation must be positive", ErrPrecondition)
	}
	return nil
}

// WrapCursor encodes a bounded driver cursor and binds it to prefix.
func WrapCursor(prefix, payload string) (string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	if payload == "" {
		return "", nil
	}
	if len(payload) > MaxCursorPayloadBytes {
		return "", fmt.Errorf("%w: cursor payload is %d bytes, limit is %d", ErrPrecondition, len(payload), MaxCursorPayloadBytes)
	}

	envelope := make([]byte, cursorHeaderBytes+len(prefix)+len(payload))
	envelope[0] = cursorVersion
	binary.BigEndian.PutUint16(envelope[1:cursorHeaderBytes], uint16(len(prefix)))
	copy(envelope[cursorHeaderBytes:], prefix)
	copy(envelope[cursorHeaderBytes+len(prefix):], payload)
	return base64.RawURLEncoding.EncodeToString(envelope), nil
}

// UnwrapCursor validates a cursor envelope and returns its driver payload.
func UnwrapCursor(prefix, cursor string) (string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > maxEncodedCursorBytes {
		return "", invalidCursor()
	}

	envelope, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(envelope) < cursorHeaderBytes+1 || len(envelope) > maxCursorEnvelopeBytes || envelope[0] != cursorVersion {
		return "", invalidCursor()
	}
	prefixLength := int(binary.BigEndian.Uint16(envelope[1:cursorHeaderBytes]))
	payloadOffset := cursorHeaderBytes + prefixLength
	if prefixLength > MaxKeyBytes || payloadOffset >= len(envelope) || string(envelope[cursorHeaderBytes:payloadOffset]) != prefix {
		return "", invalidCursor()
	}
	payload := envelope[payloadOffset:]
	if len(payload) > MaxCursorPayloadBytes {
		return "", invalidCursor()
	}
	return string(payload), nil
}

func validateText(kind, value string) error {
	if len(value) > MaxKeyBytes {
		return fmt.Errorf("%w: %s is %d bytes, limit is %d", ErrPrecondition, kind, len(value), MaxKeyBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrPrecondition, kind)
	}
	for _, character := range value {
		if character == '\r' || character == '\n' {
			return fmt.Errorf("%w: %s contains a line break", ErrPrecondition, kind)
		}
	}
	return nil
}

func invalidCursor() error {
	return fmt.Errorf("%w: invalid list cursor", ErrPrecondition)
}
