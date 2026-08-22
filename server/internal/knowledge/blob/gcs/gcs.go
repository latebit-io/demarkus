// Package gcs implements blob storage with Google Cloud Storage.
package gcs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const contentType = "application/octet-stream"

var (
	castagnoliTable   = crc32.MakeTable(crc32.Castagnoli)
	errObjectTooLarge = errors.New("GCS object exceeds configured size limit")
)

// Store binds blob operations to one GCS bucket.
type Store struct {
	bucket         *storage.BucketHandle
	maxObjectBytes int64
}

// New binds a borrowed client to bucket without performing network I/O.
func New(client *storage.Client, bucket string, maxObjectBytes int64) (*Store, error) {
	if client == nil {
		return nil, fmt.Errorf("new GCS store: %w: client is nil", blob.ErrPrecondition)
	}
	if err := validateBucket(bucket); err != nil {
		return nil, fmt.Errorf("new GCS store: %w", err)
	}
	if maxObjectBytes <= 0 {
		return nil, fmt.Errorf("new GCS store: %w: max object bytes must be positive", blob.ErrPrecondition)
	}

	// RetryNever suppresses request-level SDK retries, but a GCS Reader may resume
	// a failed body at the pinned generation. This is safe for reads and forbidden
	// for mutations.
	bucketHandle := client.Bucket(bucket).Retryer(storage.WithPolicy(storage.RetryNever))
	return &Store{bucket: bucketHandle, maxObjectBytes: maxObjectBytes}, nil
}

// Get returns the current object and validates its provider checksum.
func (s *Store) Get(ctx context.Context, key string) (blob.Object, error) {
	const op = "get"
	if err := preflight(ctx, op, key, blob.ValidateKey(key)); err != nil {
		return blob.Object{}, err
	}

	reader, err := s.object(key).NewReader(ctx)
	if err != nil {
		return blob.Object{}, classifyProviderError(ctx, op, key, err)
	}
	attributes, attrErr := s.readerAttributes(key, &reader.Attrs)
	if attrErr != nil {
		return blob.Object{}, integrityError(ctx, op, key, errors.Join(attrErr, reader.Close()), false)
	}

	data, readErr := readBounded(reader, s.maxObjectBytes)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return blob.Object{}, classifyProviderError(ctx, op, key, err)
	}
	if int64(len(data)) != attributes.Size {
		cause := fmt.Errorf("provider size is %d, read %d bytes", attributes.Size, len(data))
		return blob.Object{}, integrityError(ctx, op, key, cause, false)
	}
	if checksum := crc32c(data); checksum != reader.Attrs.CRC32C {
		cause := fmt.Errorf("provider CRC32C is %d, computed %d", reader.Attrs.CRC32C, checksum)
		return blob.Object{}, integrityError(ctx, op, key, cause, false)
	}
	return blob.Object{Data: data, Attributes: attributes}, nil
}

// Head returns attributes for the current object generation.
func (s *Store) Head(ctx context.Context, key string) (blob.Attributes, error) {
	const op = "head"
	if err := preflight(ctx, op, key, blob.ValidateKey(key)); err != nil {
		return blob.Attributes{}, err
	}

	providerAttributes, err := s.object(key).Attrs(ctx)
	if err != nil {
		return blob.Attributes{}, classifyProviderError(ctx, op, key, err)
	}
	attributes, err := s.objectAttributes(providerAttributes, key)
	if err != nil {
		return blob.Attributes{}, integrityError(ctx, op, key, err, false)
	}
	return attributes, nil
}

// Create stores a new object with a does-not-exist precondition.
func (s *Store) Create(ctx context.Context, key string, data []byte) (blob.Attributes, error) {
	const op = "create"
	if err := preflight(ctx, op, key, blob.ValidateKey(key)); err != nil {
		return blob.Attributes{}, err
	}
	if err := s.validateWriteSize(data); err != nil {
		return blob.Attributes{}, operationError(op, key, err)
	}

	object := s.object(key).If(storage.Conditions{DoesNotExist: true})
	return s.write(ctx, op, key, object, bytes.Clone(data))
}

// Replace stores a new object revision when generation matches exactly.
func (s *Store) Replace(ctx context.Context, key string, generation blob.Generation, data []byte) (blob.Attributes, error) {
	const op = "replace"
	if err := preflight(ctx, op, key, blob.ValidateKey(key), blob.ValidateGeneration(generation)); err != nil {
		return blob.Attributes{}, err
	}
	if err := s.validateWriteSize(data); err != nil {
		return blob.Attributes{}, operationError(op, key, err)
	}

	object := s.object(key).If(storage.Conditions{GenerationMatch: int64(generation)})
	return s.write(ctx, op, key, object, bytes.Clone(data))
}

// Delete removes the current object when generation matches exactly.
func (s *Store) Delete(ctx context.Context, key string, generation blob.Generation) error {
	const op = "delete"
	if err := preflight(ctx, op, key, blob.ValidateKey(key), blob.ValidateGeneration(generation)); err != nil {
		return err
	}

	object := s.object(key).If(storage.Conditions{GenerationMatch: int64(generation)})
	if err := object.Delete(ctx); err != nil {
		return classifyProviderError(ctx, op, key, err)
	}
	return nil
}

// List returns one provider-ordered page under prefix.
func (s *Store) List(ctx context.Context, prefix, cursor string) (blob.ListResult, error) {
	const op = "list"
	if err := preflight(ctx, op, prefix, blob.ValidatePrefix(prefix)); err != nil {
		return blob.ListResult{}, err
	}
	providerCursor, err := blob.UnwrapCursor(prefix, cursor)
	if err != nil {
		return blob.ListResult{}, operationError(op, prefix, err)
	}

	query := &storage.Query{Prefix: prefix, Projection: storage.ProjectionNoACL}
	if err := query.SetAttrSelection([]string{"Name", "Generation", "Size", "Updated"}); err != nil {
		return blob.ListResult{}, operationError(op, prefix, err)
	}
	objects := s.bucket.Objects(ctx, query)
	pageInfo := objects.PageInfo()
	pageInfo.Token = providerCursor
	pageInfo.MaxSize = blob.MaxListPage
	first, err := objects.Next()
	if errors.Is(err, iterator.Done) {
		return blob.ListResult{}, nil
	}
	if err != nil {
		return blob.ListResult{}, classifyProviderError(ctx, op, prefix, err)
	}
	remaining := pageInfo.Remaining()
	if remaining >= blob.MaxListPage {
		cause := fmt.Errorf("provider returned at least %d objects, page limit is %d", remaining+1, blob.MaxListPage)
		return blob.ListResult{}, integrityError(ctx, op, prefix, cause, false)
	}
	providerAttributes := make([]*storage.ObjectAttrs, 1, remaining+1)
	providerAttributes[0] = first
	for range remaining {
		providerAttribute, err := objects.Next()
		if err != nil {
			return blob.ListResult{}, classifyProviderError(ctx, op, prefix, err)
		}
		providerAttributes = append(providerAttributes, providerAttribute)
	}

	nextCursor, err := blob.WrapCursor(prefix, pageInfo.Token)
	if err != nil {
		return blob.ListResult{}, integrityError(ctx, op, prefix, fmt.Errorf("encode provider cursor: %w", err), false)
	}
	result := blob.ListResult{Objects: make([]blob.Attributes, len(providerAttributes)), NextCursor: nextCursor}
	previous := ""
	for index, providerAttribute := range providerAttributes {
		attributes, err := s.objectAttributes(providerAttribute, "")
		if err != nil {
			return blob.ListResult{}, integrityError(ctx, op, prefix, err, false)
		}
		if !strings.HasPrefix(attributes.Key, prefix) {
			cause := fmt.Errorf("provider key %q is outside prefix %q", attributes.Key, prefix)
			return blob.ListResult{}, integrityError(ctx, op, prefix, cause, false)
		}
		if index > 0 && attributes.Key <= previous {
			cause := fmt.Errorf("provider keys %q and %q are not strictly ordered", previous, attributes.Key)
			return blob.ListResult{}, integrityError(ctx, op, prefix, cause, false)
		}
		result.Objects[index] = attributes
		previous = attributes.Key
	}
	return result, nil
}

func (s *Store) object(key string) *storage.ObjectHandle {
	return s.bucket.Object(key).Retryer(storage.WithPolicy(storage.RetryNever))
}

func (s *Store) write(
	ctx context.Context,
	op, key string,
	object *storage.ObjectHandle,
	data []byte,
) (blob.Attributes, error) {
	checksum := crc32c(data)
	writer := object.NewWriter(ctx)
	writer.ChunkSize = 0
	writer.ContentType = contentType
	writer.CRC32C = checksum
	writer.SendCRC32C = true

	written, writeErr := writer.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	closeErr := writer.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		if errors.Is(err, io.ErrShortWrite) {
			return blob.Attributes{}, integrityError(ctx, op, key, err, true)
		}
		return blob.Attributes{}, classifyProviderError(ctx, op, key, err)
	}

	providerAttributes := writer.Attrs()
	attributes, err := s.objectAttributes(providerAttributes, key)
	if err == nil && attributes.Size != int64(len(data)) {
		err = fmt.Errorf("provider size is %d, wrote %d bytes", attributes.Size, len(data))
	}
	if err == nil && providerAttributes.CRC32C != checksum {
		err = fmt.Errorf("provider CRC32C is %d, sent %d", providerAttributes.CRC32C, checksum)
	}
	if err != nil {
		return blob.Attributes{}, integrityError(ctx, op, key, err, true)
	}
	return attributes, nil
}

func (s *Store) validateWriteSize(data []byte) error {
	if int64(len(data)) > s.maxObjectBytes {
		return fmt.Errorf("%w: object is %d bytes, limit is %d", blob.ErrPrecondition, len(data), s.maxObjectBytes)
	}
	return nil
}

func (s *Store) objectAttributes(provider *storage.ObjectAttrs, expectedKey string) (blob.Attributes, error) {
	if provider == nil {
		return blob.Attributes{}, errors.New("provider attributes are nil")
	}
	if err := blob.ValidateKey(provider.Name); err != nil {
		return blob.Attributes{}, fmt.Errorf("provider key %q is invalid: %v", provider.Name, err)
	}
	if expectedKey != "" && provider.Name != expectedKey {
		return blob.Attributes{}, fmt.Errorf("provider key is %q, requested %q", provider.Name, expectedKey)
	}
	return s.validatedAttributes(provider.Name, provider.Generation, provider.Size, provider.Updated)
}

func (s *Store) readerAttributes(key string, provider *storage.ReaderObjectAttrs) (blob.Attributes, error) {
	return s.validatedAttributes(key, provider.Generation, provider.Size, provider.LastModified)
}

func (s *Store) validatedAttributes(key string, generation, size int64, modified time.Time) (blob.Attributes, error) {
	if generation <= 0 {
		return blob.Attributes{}, fmt.Errorf("provider generation is %d", generation)
	}
	if size < 0 {
		return blob.Attributes{}, fmt.Errorf("provider size is %d", size)
	}
	if size > s.maxObjectBytes {
		return blob.Attributes{}, fmt.Errorf("provider size is %d, limit is %d", size, s.maxObjectBytes)
	}
	if modified.IsZero() {
		return blob.Attributes{}, errors.New("provider modified time is zero")
	}
	return blob.Attributes{
		Key:        key,
		Generation: blob.Generation(generation),
		Size:       size,
		Modified:   modified.UTC().Truncate(time.Second),
	}, nil
}

func preflight(ctx context.Context, op, key string, validations ...error) error {
	if err := ctx.Err(); err != nil {
		return operationError(op, key, err)
	}
	for _, err := range validations {
		if err != nil {
			return operationError(op, key, err)
		}
	}
	return nil
}

func classifyProviderError(ctx context.Context, op, key string, cause error) error {
	httpStatus := providerStatus(cause)
	grpcCode, hasGRPCCode := providerGRPCCode(cause)
	notFound := httpStatus == http.StatusNotFound || errors.Is(cause, storage.ErrObjectNotExist)
	classification := classifyHTTPProviderError(op, httpStatus, notFound)
	if classification.category == nil && !classification.permanent && hasGRPCCode {
		classification = classifyGRPCProviderError(op, grpcCode)
	}
	if classification.category == nil && isIntegrityFailure(cause) {
		classification.category = blob.ErrIntegrity
		classification.permanent = false
	}
	if classification.permanent {
		return operationError(op, key, joinContext(ctx, cause))
	}
	if classification.category == nil {
		classification.category = blob.ErrUnavailable
	}
	ambiguous := isMutation(op) && classification.category != blob.ErrPrecondition

	errorsToJoin := []error{classification.category}
	if ambiguous {
		errorsToJoin = append(errorsToJoin, blob.ErrAmbiguous)
	}
	errorsToJoin = append(errorsToJoin, cause)
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(cause, contextErr) {
		errorsToJoin = append(errorsToJoin, contextErr)
	}
	return operationError(op, key, errors.Join(errorsToJoin...))
}

func integrityError(ctx context.Context, op, key string, cause error, ambiguous bool) error {
	errorsToJoin := []error{blob.ErrIntegrity}
	if ambiguous {
		errorsToJoin = append(errorsToJoin, blob.ErrAmbiguous)
	}
	if cause != nil {
		errorsToJoin = append(errorsToJoin, cause)
	}
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(cause, contextErr) {
		errorsToJoin = append(errorsToJoin, contextErr)
	}
	return operationError(op, key, errors.Join(errorsToJoin...))
}

func providerStatus(err error) int {
	var providerError *googleapi.Error
	if errors.As(err, &providerError) {
		return providerError.Code
	}
	return 0
}

func providerGRPCCode(err error) (codes.Code, bool) {
	provider, ok := grpcstatus.FromError(err)
	if !ok {
		return codes.Unknown, false
	}
	return provider.Code(), true
}

type providerClassification struct {
	category  error
	permanent bool
}

func classifyHTTPProviderError(op string, status int, notFound bool) providerClassification {
	if notFound {
		category := notFoundCategory(op)
		return providerClassification{category: category, permanent: category == nil}
	}
	switch {
	case op == "list" && status == http.StatusBadRequest:
		return providerClassification{category: blob.ErrPrecondition}
	case isMutation(op) && status == http.StatusPreconditionFailed:
		return providerClassification{category: blob.ErrPrecondition}
	case status == http.StatusTooManyRequests:
		return providerClassification{category: blob.ErrThrottled}
	case status == http.StatusRequestTimeout || status >= 500 && status <= 599:
		return providerClassification{category: blob.ErrUnavailable}
	case status != 0:
		return providerClassification{permanent: true}
	default:
		return providerClassification{}
	}
}

func classifyGRPCProviderError(op string, code codes.Code) providerClassification {
	switch code {
	case codes.NotFound:
		category := notFoundCategory(op)
		return providerClassification{category: category, permanent: category == nil}
	case codes.FailedPrecondition:
		if isMutation(op) || op == "list" {
			return providerClassification{category: blob.ErrPrecondition}
		}
		return providerClassification{permanent: true}
	case codes.AlreadyExists:
		if op == "create" {
			return providerClassification{category: blob.ErrPrecondition}
		}
		return providerClassification{permanent: true}
	case codes.ResourceExhausted:
		return providerClassification{category: blob.ErrThrottled}
	case codes.DeadlineExceeded, codes.Unavailable, codes.Internal, codes.Aborted, codes.Canceled:
		return providerClassification{category: blob.ErrUnavailable}
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated, codes.OutOfRange:
		return providerClassification{permanent: true}
	default:
		return providerClassification{}
	}
}

func notFoundCategory(op string) error {
	switch op {
	case "get", "head":
		return blob.ErrNotFound
	case "replace", "delete":
		return blob.ErrPrecondition
	default:
		return nil
	}
}

func isIntegrityFailure(err error) bool {
	if errors.Is(err, errObjectTooLarge) || errors.Is(err, io.ErrShortWrite) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "bad crc on read") ||
		strings.Contains(message, "checksum mismatch") ||
		strings.Contains(message, "does not match the expected crc32c") ||
		strings.Contains(message, "crc32c") && strings.Contains(message, "does not match")
}

func isMutation(op string) bool {
	return op == "create" || op == "replace" || op == "delete"
}

func joinContext(ctx context.Context, cause error) error {
	if contextErr := ctx.Err(); contextErr != nil && !errors.Is(cause, contextErr) {
		return errors.Join(cause, contextErr)
	}
	return cause
}

func operationError(op, key string, err error) error {
	return &blob.OpError{Op: op, Key: key, Err: err}
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) < maximum {
		return data, nil
	}
	extra, err := io.ReadAll(io.LimitReader(reader, 1))
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		return nil, fmt.Errorf("%w: limit is %d bytes", errObjectTooLarge, maximum)
	}
	return data, nil
}

func crc32c(data []byte) uint32 {
	return crc32.Checksum(data, castagnoliTable)
}

func validateBucket(name string) error {
	maximumLength := 63
	if strings.Contains(name, ".") {
		maximumLength = 222
	}
	if len(name) < 3 || len(name) > maximumLength {
		return fmt.Errorf("%w: bucket name length must be between 3 and %d bytes", blob.ErrPrecondition, maximumLength)
	}
	if !asciiAlphanumeric(name[0]) || !asciiAlphanumeric(name[len(name)-1]) {
		return fmt.Errorf("%w: invalid bucket name %q", blob.ErrPrecondition, name)
	}
	componentStart := 0
	for index := range len(name) {
		character := name[index]
		if character == '.' {
			if index == componentStart || index-componentStart > 63 || !asciiAlphanumeric(name[index-1]) {
				return fmt.Errorf("%w: invalid bucket name %q", blob.ErrPrecondition, name)
			}
			componentStart = index + 1
			continue
		}
		if index == componentStart && !asciiAlphanumeric(character) {
			return fmt.Errorf("%w: invalid bucket name %q", blob.ErrPrecondition, name)
		}
		if !asciiAlphanumeric(character) && character != '-' && character != '_' {
			return fmt.Errorf("%w: invalid bucket name %q", blob.ErrPrecondition, name)
		}
	}
	if len(name)-componentStart > 63 || net.ParseIP(name) != nil || reservedBucketName(name) {
		return fmt.Errorf("%w: invalid bucket name %q", blob.ErrPrecondition, name)
	}
	return nil
}

func reservedBucketName(name string) bool {
	return strings.HasPrefix(name, "goog") ||
		strings.Contains(name, "google") ||
		strings.Contains(name, "g00gle") ||
		strings.Contains(name, "go0gle") ||
		strings.Contains(name, "g0ogle")
}

func asciiAlphanumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

var _ blob.Store = (*Store)(nil)
