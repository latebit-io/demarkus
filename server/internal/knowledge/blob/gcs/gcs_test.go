package gcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const testBucket = "demarkus-test-bucket"

var blobCategories = []error{
	blob.ErrNotFound,
	blob.ErrPrecondition,
	blob.ErrThrottled,
	blob.ErrUnavailable,
	blob.ErrIntegrity,
	blob.ErrAmbiguous,
}

func TestNew(t *testing.T) {
	var attempts atomic.Int64
	client := newTestClient(t, "https://storage.test/storage/v1/", roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("unexpected request")
	}))

	tests := []struct {
		name           string
		client         *storage.Client
		bucket         string
		maximum        int64
		wantError      error
		wantBucketName string
	}{
		{name: "valid", client: client, bucket: testBucket, maximum: 1, wantBucketName: testBucket},
		{name: "nil client", bucket: testBucket, maximum: 1, wantError: blob.ErrPrecondition},
		{name: "invalid bucket", client: client, bucket: "UPPER", maximum: 1, wantError: blob.ErrPrecondition},
		{name: "zero maximum", client: client, bucket: testBucket, maximum: 0, wantError: blob.ErrPrecondition},
		{name: "negative maximum", client: client, bucket: testBucket, maximum: -1, wantError: blob.ErrPrecondition},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(test.client, test.bucket, test.maximum)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) || store != nil {
					t.Fatalf("New() = (%v, %v), want nil %v", store, err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			if store.bucket.BucketName() != test.wantBucketName || store.maxObjectBytes != test.maximum {
				t.Errorf("store = %+v, want bucket %q maximum %d", store, test.wantBucketName, test.maximum)
			}
		})
	}
	if attempts.Load() != 0 {
		t.Errorf("New made %d network requests, want 0", attempts.Load())
	}
}

func TestValidateBucket(t *testing.T) {
	maximumDotted := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 30)
	tests := []struct {
		name    string
		bucket  string
		wantErr bool
	}{
		{name: "minimum", bucket: "abc"},
		{name: "allowed punctuation", bucket: "a-b_c"},
		{name: "dotted", bucket: "bucket.example"},
		{name: "maximum plain", bucket: strings.Repeat("a", 63)},
		{name: "maximum dotted", bucket: maximumDotted},
		{name: "empty", bucket: "", wantErr: true},
		{name: "too short", bucket: "ab", wantErr: true},
		{name: "plain too long", bucket: strings.Repeat("a", 64), wantErr: true},
		{name: "dotted too long", bucket: maximumDotted + "e", wantErr: true},
		{name: "component too long", bucket: strings.Repeat("a", 64) + ".b", wantErr: true},
		{name: "empty component", bucket: "abc..def", wantErr: true},
		{name: "component starts punctuation", bucket: "abc.-def", wantErr: true},
		{name: "component ends punctuation", bucket: "abc-.def", wantErr: true},
		{name: "starts punctuation", bucket: "-abc", wantErr: true},
		{name: "ends punctuation", bucket: "abc_", wantErr: true},
		{name: "uppercase", bucket: "Abc", wantErr: true},
		{name: "invalid character", bucket: "abc/def", wantErr: true},
		{name: "IPv4 address", bucket: "192.168.5.4", wantErr: true},
		{name: "goog prefix", bucket: "goog-bucket", wantErr: true},
		{name: "google", bucket: "my-google-bucket", wantErr: true},
		{name: "g00gle", bucket: "my-g00gle-bucket", wantErr: true},
		{name: "go0gle", bucket: "my-go0gle-bucket", wantErr: true},
		{name: "g0ogle", bucket: "my-g0ogle-bucket", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateBucket(test.bucket)
			if test.wantErr {
				if !errors.Is(err, blob.ErrPrecondition) {
					t.Fatalf("validateBucket(%q) = %v, want ErrPrecondition", test.bucket, err)
				}
				return
			}
			if err != nil {
				t.Errorf("validateBucket(%q): %v", test.bucket, err)
			}
		})
	}
}

func TestAttributeValidation(t *testing.T) {
	modified := time.Date(2026, time.August, 22, 3, 4, 5, 6, time.FixedZone("test", -7*60*60))
	valid := func() *storage.ObjectAttrs {
		return &storage.ObjectAttrs{Name: "key", Generation: 7, Size: 3, Updated: modified}
	}
	tests := []struct {
		name        string
		provider    *storage.ObjectAttrs
		expectedKey string
	}{
		{name: "nil", expectedKey: "key"},
		{name: "empty key", provider: &storage.ObjectAttrs{Generation: 7, Size: 3, Updated: modified}, expectedKey: "key"},
		{name: "invalid UTF-8 key", provider: &storage.ObjectAttrs{Name: string([]byte{0xff}), Generation: 7, Size: 3, Updated: modified}},
		{name: "wrong key", provider: valid(), expectedKey: "other"},
		{name: "zero generation", provider: &storage.ObjectAttrs{Name: "key", Size: 3, Updated: modified}, expectedKey: "key"},
		{name: "negative generation", provider: &storage.ObjectAttrs{Name: "key", Generation: -1, Size: 3, Updated: modified}, expectedKey: "key"},
		{name: "negative size", provider: &storage.ObjectAttrs{Name: "key", Generation: 7, Size: -1, Updated: modified}, expectedKey: "key"},
		{name: "oversized", provider: &storage.ObjectAttrs{Name: "key", Generation: 7, Size: 4, Updated: modified}, expectedKey: "key"},
		{name: "zero modified", provider: &storage.ObjectAttrs{Name: "key", Generation: 7, Size: 3}, expectedKey: "key"},
	}
	store := &Store{maxObjectBytes: 3}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attributes, err := store.objectAttributes(test.provider, test.expectedKey)
			if err == nil {
				t.Fatal("objectAttributes() succeeded")
			}
			if attributes != (blob.Attributes{}) {
				t.Errorf("attributes = %+v, want zero", attributes)
			}
		})
	}

	t.Run("object valid", func(t *testing.T) {
		attributes, err := store.objectAttributes(valid(), "key")
		if err != nil {
			t.Fatalf("objectAttributes(): %v", err)
		}
		want := blob.Attributes{Key: "key", Generation: 7, Size: 3, Modified: modified.UTC().Truncate(time.Second)}
		if attributes != want || attributes.Modified.Location() != time.UTC {
			t.Errorf("attributes = %+v, want %+v in UTC", attributes, want)
		}
	})

	t.Run("reader valid", func(t *testing.T) {
		provider := storage.ReaderObjectAttrs{Generation: 8, Size: 2, LastModified: modified}
		attributes, err := store.readerAttributes("reader-key", &provider)
		if err != nil {
			t.Fatalf("readerAttributes(): %v", err)
		}
		want := blob.Attributes{Key: "reader-key", Generation: 8, Size: 2, Modified: modified.UTC().Truncate(time.Second)}
		if attributes != want {
			t.Errorf("attributes = %+v, want %+v", attributes, want)
		}
	})
}

func TestWriteBounds(t *testing.T) {
	store := &Store{maxObjectBytes: 3}
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{name: "empty", data: nil},
		{name: "exact", data: []byte("123")},
		{name: "oversized", data: []byte("1234"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := store.validateWriteSize(test.data)
			if test.wantErr != errors.Is(err, blob.ErrPrecondition) {
				t.Errorf("validateWriteSize(%d) = %v", len(test.data), err)
			}
		})
	}

	var attempts atomic.Int64
	client := newTestClient(t, "https://storage.test/storage/v1/", roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("unexpected request")
	}))
	bounded, err := New(client, testBucket, 3)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	attributes, err := bounded.Create(context.Background(), "key", []byte("1234"))
	assertOperationError(t, err, nil, "create", "key", blob.ErrPrecondition)
	assertZeroAttributes(t, attributes)
	attributes, err = bounded.Replace(context.Background(), "key", 1, []byte("1234"))
	assertOperationError(t, err, nil, "replace", "key", blob.ErrPrecondition)
	assertZeroAttributes(t, attributes)
	if attempts.Load() != 0 {
		t.Errorf("oversized writes made %d requests, want 0", attempts.Load())
	}
}

func TestContextPreflight(t *testing.T) {
	var attempts atomic.Int64
	client := newTestClient(t, "https://storage.test/storage/v1/", roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		return nil, errors.New("unexpected request")
	}))
	store, err := New(client, testBucket, 3)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		op   string
		key  string
		run  func(*testing.T) error
	}{
		{name: "Get", op: "get", key: "key", run: func(t *testing.T) error {
			object, err := store.Get(ctx, "key")
			assertZeroObject(t, &object)
			return err
		}},
		{name: "Head", op: "head", key: "key", run: func(t *testing.T) error {
			attributes, err := store.Head(ctx, "key")
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Create", op: "create", key: "key", run: func(t *testing.T) error {
			attributes, err := store.Create(ctx, "key", []byte("1234"))
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Replace", op: "replace", key: "key", run: func(t *testing.T) error {
			attributes, err := store.Replace(ctx, "key", 1, []byte("1234"))
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Delete", op: "delete", key: "key", run: func(*testing.T) error {
			return store.Delete(ctx, "key", 1)
		}},
		{name: "List", op: "list", key: "prefix/", run: func(t *testing.T) error {
			result, err := store.List(ctx, "prefix/", "cursor")
			assertZeroListResult(t, result)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertOperationError(t, test.run(t), nil, test.op, test.key, context.Canceled)
		})
	}

	t.Run("context wins validation", func(t *testing.T) {
		attributes, err := store.Replace(ctx, "", 0, []byte("1234"))
		assertZeroAttributes(t, attributes)
		assertOperationError(t, err, nil, "replace", "", context.Canceled)
		if errors.Is(err, blob.ErrPrecondition) {
			t.Errorf("error = %v, context must precede input validation", err)
		}
	})
	if attempts.Load() != 0 {
		t.Errorf("canceled operations made %d requests, want 0", attempts.Load())
	}
}

func TestProviderErrorClassification(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	canceled, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()
	tests := []struct {
		name        string
		ctx         context.Context
		op          string
		cause       error
		want        []error
		wantContext error
	}{
		{name: "create 412", ctx: context.Background(), op: "create", cause: apiError(http.StatusPreconditionFailed), want: []error{blob.ErrPrecondition}},
		{name: "replace 412", ctx: context.Background(), op: "replace", cause: apiError(http.StatusPreconditionFailed), want: []error{blob.ErrPrecondition}},
		{name: "delete 412", ctx: context.Background(), op: "delete", cause: apiError(http.StatusPreconditionFailed), want: []error{blob.ErrPrecondition}},
		{name: "412 precedes expired context", ctx: expired, op: "replace", cause: apiError(http.StatusPreconditionFailed), want: []error{blob.ErrPrecondition}, wantContext: context.DeadlineExceeded},
		{name: "412 precedes canceled context", ctx: canceled, op: "delete", cause: apiError(http.StatusPreconditionFailed), want: []error{blob.ErrPrecondition}, wantContext: context.Canceled},
		{name: "get 404", ctx: context.Background(), op: "get", cause: apiError(http.StatusNotFound), want: []error{blob.ErrNotFound}},
		{name: "head missing sentinel", ctx: context.Background(), op: "head", cause: storage.ErrObjectNotExist, want: []error{blob.ErrNotFound}},
		{name: "replace 404", ctx: context.Background(), op: "replace", cause: apiError(http.StatusNotFound), want: []error{blob.ErrPrecondition}},
		{name: "delete missing sentinel", ctx: context.Background(), op: "delete", cause: storage.ErrObjectNotExist, want: []error{blob.ErrPrecondition}},
		{name: "create 404", ctx: context.Background(), op: "create", cause: apiError(http.StatusNotFound)},
		{name: "list 404", ctx: context.Background(), op: "list", cause: apiError(http.StatusNotFound)},
		{name: "list bad cursor", ctx: context.Background(), op: "list", cause: apiError(http.StatusBadRequest), want: []error{blob.ErrPrecondition}},
		{name: "create bad request", ctx: context.Background(), op: "create", cause: apiError(http.StatusBadRequest)},
		{name: "get 429", ctx: context.Background(), op: "get", cause: apiError(http.StatusTooManyRequests), want: []error{blob.ErrThrottled}},
		{name: "create 429", ctx: context.Background(), op: "create", cause: apiError(http.StatusTooManyRequests), want: []error{blob.ErrThrottled, blob.ErrAmbiguous}},
		{name: "head 408", ctx: context.Background(), op: "head", cause: apiError(http.StatusRequestTimeout), want: []error{blob.ErrUnavailable}},
		{name: "replace 408", ctx: context.Background(), op: "replace", cause: apiError(http.StatusRequestTimeout), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "list 500", ctx: context.Background(), op: "list", cause: apiError(http.StatusInternalServerError), want: []error{blob.ErrUnavailable}},
		{name: "delete 503", ctx: context.Background(), op: "delete", cause: apiError(http.StatusServiceUnavailable), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "create 599", ctx: context.Background(), op: "create", cause: apiError(599), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "permanent 403", ctx: context.Background(), op: "delete", cause: apiError(http.StatusForbidden)},
		{name: "CRC read", ctx: context.Background(), op: "get", cause: errors.New("storage: bad CRC on read"), want: []error{blob.ErrIntegrity}},
		{name: "CRC write rejection", ctx: context.Background(), op: "create", cause: errors.New("checksum mismatch"), want: []error{blob.ErrIntegrity, blob.ErrAmbiguous}},
		{name: "CRC write 400", ctx: context.Background(), op: "create", cause: &googleapi.Error{Code: http.StatusBadRequest, Message: "CRC32C does not match"}, want: []error{blob.ErrIntegrity, blob.ErrAmbiguous}},
		{name: "oversized read", ctx: context.Background(), op: "get", cause: errObjectTooLarge, want: []error{blob.ErrIntegrity}},
		{name: "unexpected EOF read", ctx: context.Background(), op: "get", cause: io.ErrUnexpectedEOF, want: []error{blob.ErrUnavailable}},
		{name: "unexpected EOF write", ctx: context.Background(), op: "replace", cause: io.ErrUnexpectedEOF, want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "transport", ctx: context.Background(), op: "head", cause: errors.New("connection reset"), want: []error{blob.ErrUnavailable}},
		{name: "transport mutation", ctx: context.Background(), op: "delete", cause: errors.New("connection reset"), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "network timeout", ctx: context.Background(), op: "get", cause: timeoutFailure{}, want: []error{blob.ErrUnavailable}},
		{name: "deadline mutation", ctx: context.Background(), op: "create", cause: context.DeadlineExceeded, want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}, wantContext: context.DeadlineExceeded},
		{name: "gRPC get not found", ctx: context.Background(), op: "get", cause: grpcstatus.Error(codes.NotFound, "missing"), want: []error{blob.ErrNotFound}},
		{name: "gRPC head not found", ctx: context.Background(), op: "head", cause: grpcstatus.Error(codes.NotFound, "missing"), want: []error{blob.ErrNotFound}},
		{name: "gRPC replace not found", ctx: context.Background(), op: "replace", cause: grpcstatus.Error(codes.NotFound, "missing"), want: []error{blob.ErrPrecondition}},
		{name: "gRPC delete not found", ctx: context.Background(), op: "delete", cause: grpcstatus.Error(codes.NotFound, "missing"), want: []error{blob.ErrPrecondition}},
		{name: "gRPC create not found", ctx: context.Background(), op: "create", cause: grpcstatus.Error(codes.NotFound, "bucket missing")},
		{name: "gRPC create already exists", ctx: context.Background(), op: "create", cause: grpcstatus.Error(codes.AlreadyExists, "exists"), want: []error{blob.ErrPrecondition}},
		{name: "gRPC replace failed precondition", ctx: context.Background(), op: "replace", cause: grpcstatus.Error(codes.FailedPrecondition, "stale"), want: []error{blob.ErrPrecondition}},
		{name: "gRPC list failed precondition", ctx: context.Background(), op: "list", cause: grpcstatus.Error(codes.FailedPrecondition, "cursor"), want: []error{blob.ErrPrecondition}},
		{name: "gRPC read exhausted", ctx: context.Background(), op: "get", cause: grpcstatus.Error(codes.ResourceExhausted, "quota"), want: []error{blob.ErrThrottled}},
		{name: "gRPC mutation exhausted", ctx: context.Background(), op: "create", cause: grpcstatus.Error(codes.ResourceExhausted, "quota"), want: []error{blob.ErrThrottled, blob.ErrAmbiguous}},
		{name: "gRPC deadline", ctx: context.Background(), op: "head", cause: grpcstatus.Error(codes.DeadlineExceeded, "deadline"), want: []error{blob.ErrUnavailable}},
		{name: "gRPC unavailable mutation", ctx: context.Background(), op: "replace", cause: grpcstatus.Error(codes.Unavailable, "unavailable"), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "gRPC internal", ctx: context.Background(), op: "list", cause: grpcstatus.Error(codes.Internal, "internal"), want: []error{blob.ErrUnavailable}},
		{name: "gRPC aborted mutation", ctx: context.Background(), op: "delete", cause: grpcstatus.Error(codes.Aborted, "aborted"), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "gRPC canceled", ctx: context.Background(), op: "get", cause: grpcstatus.Error(codes.Canceled, "canceled"), want: []error{blob.ErrUnavailable}},
		{name: "gRPC canceled mutation", ctx: context.Background(), op: "create", cause: grpcstatus.Error(codes.Canceled, "canceled"), want: []error{blob.ErrUnavailable, blob.ErrAmbiguous}},
		{name: "gRPC permanent invalid argument", ctx: context.Background(), op: "delete", cause: grpcstatus.Error(codes.InvalidArgument, "invalid")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := classifyProviderError(test.ctx, test.op, "key", test.cause)
			assertOperationError(t, err, test.cause, test.op, "key", test.want...)
			if test.wantContext != nil && !errors.Is(err, test.wantContext) {
				t.Errorf("error = %v, want context error %v", err, test.wantContext)
			}
		})
	}
}

func TestReadBounded(t *testing.T) {
	readFailure := errors.New("read failure")
	tests := []struct {
		name        string
		reader      io.Reader
		maximum     int64
		want        []byte
		wantError   error
		wantNilData bool
	}{
		{name: "smaller", reader: strings.NewReader("12"), maximum: 3, want: []byte("12")},
		{name: "exact", reader: strings.NewReader("123"), maximum: 3, want: []byte("123")},
		{name: "oversized", reader: strings.NewReader("1234"), maximum: 3, wantError: errObjectTooLarge, wantNilData: true},
		{name: "source error", reader: &dataErrorReader{data: []byte("12"), err: readFailure}, maximum: 3, wantError: readFailure, wantNilData: true},
		{name: "probe error", reader: io.MultiReader(strings.NewReader("123"), errorReader{err: readFailure}), maximum: 3, wantError: readFailure, wantNilData: true},
		{name: "zero empty", reader: strings.NewReader(""), maximum: 0, want: []byte{}},
		{name: "zero oversized", reader: strings.NewReader("1"), maximum: 0, wantError: errObjectTooLarge, wantNilData: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := readBounded(test.reader, test.maximum)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("readBounded() error = %v, want %v", err, test.wantError)
			}
			if test.wantNilData {
				if data != nil {
					t.Errorf("data = %q, want nil", data)
				}
				return
			}
			if !bytes.Equal(data, test.want) {
				t.Errorf("data = %q, want %q", data, test.want)
			}
		})
	}

	t.Run("consumes at most limit plus one", func(t *testing.T) {
		reader := strings.NewReader("12345678")
		data, err := readBounded(reader, 3)
		if !errors.Is(err, errObjectTooLarge) || data != nil {
			t.Fatalf("readBounded() = (%q, %v)", data, err)
		}
		if reader.Len() != 4 {
			t.Errorf("remaining bytes = %d, want 4", reader.Len())
		}
	})
}

func TestCRC32C(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want uint32
	}{
		{name: "empty", data: nil, want: 0},
		{name: "Castagnoli check vector", data: []byte("123456789"), want: 0xe3069283},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := crc32c(test.data); got != test.want {
				t.Errorf("crc32c(%q) = %#x, want %#x", test.data, got, test.want)
			}
		})
	}
}

func TestReadRequests(t *testing.T) {
	data := []byte("object data")
	modified := time.Date(2026, time.August, 22, 10, 11, 12, 0, time.UTC)
	requests := make(chan capturedRequest, 2)
	store := newHTTPTestStore(t, 1<<20, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests <- captureRequest(request)
		if request.URL.Query().Get("alt") == "media" {
			writeMediaResponse(t, response, data, 7, modified, gcsCRC32C(data))
			return
		}
		writeObjectResponse(t, response, "object", 7, data, modified, gcsCRC32C(data))
	}))

	object, err := store.Get(context.Background(), "object")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	attributes := blob.Attributes{Key: "object", Generation: 7, Size: int64(len(data)), Modified: modified}
	if !bytes.Equal(object.Data, data) || object.Attributes != attributes {
		t.Errorf("object = %+v, want data %q attributes %+v", object, data, attributes)
	}
	head, err := store.Head(context.Background(), "object")
	if err != nil {
		t.Fatalf("Head(): %v", err)
	}
	if head != attributes {
		t.Errorf("head = %+v, want %+v", head, attributes)
	}

	getRequest := receiveRequest(t, requests)
	headRequest := receiveRequest(t, requests)
	for _, request := range []capturedRequest{getRequest, headRequest} {
		if request.method != http.MethodGet || request.path != "/storage/v1/b/"+testBucket+"/o/object" {
			t.Errorf("request = %s %s", request.method, request.path)
		}
		if request.query.Get("generation") != "" || request.query.Get("ifGenerationMatch") != "" {
			t.Errorf("request unexpectedly pinned a generation: %v", request.query)
		}
	}
	if getRequest.query.Get("alt") != "media" {
		t.Errorf("Get query = %v, want alt=media", getRequest.query)
	}
	if headRequest.query.Get("alt") != "json" {
		t.Errorf("Head query = %v, want alt=json", headRequest.query)
	}
}

func TestGetRejectsBadCRC32C(t *testing.T) {
	data := []byte("object data")
	modified := time.Date(2026, time.August, 22, 10, 11, 12, 0, time.UTC)
	tests := []struct {
		name     string
		checksum *uint32
	}{
		{name: "missing"},
		{name: "mismatch", checksum: new(crc32c([]byte("different")))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newHTTPTestStore(t, 1<<20, http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				writeMediaResponse(t, response, data, 7, modified, test.checksum)
			}))
			object, err := store.Get(context.Background(), "object")
			assertZeroObject(t, &object)
			assertOperationError(t, err, nil, "get", "object", blob.ErrIntegrity)
		})
	}
}

func TestConditionalWriteRequests(t *testing.T) {
	data := []byte("write data")
	modified := time.Date(2026, time.August, 22, 10, 11, 12, 0, time.UTC)
	tests := []struct {
		name              string
		ifGenerationMatch string
		resultGeneration  int64
		run               func(*Store) (blob.Attributes, error)
	}{
		{name: "create", ifGenerationMatch: "0", resultGeneration: 8, run: func(store *Store) (blob.Attributes, error) {
			return store.Create(context.Background(), "object", data)
		}},
		{name: "replace", ifGenerationMatch: "7", resultGeneration: 9, run: func(store *Store) (blob.Attributes, error) {
			return store.Replace(context.Background(), "object", 7, data)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan capturedRequest, 1)
			store := newHTTPTestStore(t, 1<<20, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requests <- captureRequest(request)
				writeObjectResponse(t, response, "object", test.resultGeneration, data, modified, gcsCRC32C(data))
			}))
			attributes, err := test.run(store)
			if err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			want := blob.Attributes{Key: "object", Generation: blob.Generation(test.resultGeneration), Size: int64(len(data)), Modified: modified}
			if attributes != want {
				t.Errorf("attributes = %+v, want %+v", attributes, want)
			}

			request := receiveRequest(t, requests)
			if request.method != http.MethodPost || request.path != "/upload/storage/v1/b/"+testBucket+"/o" {
				t.Errorf("request = %s %s", request.method, request.path)
			}
			if request.query.Get("ifGenerationMatch") != test.ifGenerationMatch || request.query.Get("uploadType") != "multipart" {
				t.Errorf("query = %v", request.query)
			}
			metadata, uploaded := parseMultipartUpload(t, &request)
			if metadata.Name != "object" || metadata.ContentType != contentType || metadata.CRC32C != encodeGCSCRC32C(crc32c(data)) {
				t.Errorf("metadata = %+v", metadata)
			}
			if !bytes.Equal(uploaded, data) {
				t.Errorf("uploaded data = %q, want %q", uploaded, data)
			}
		})
	}
}

func TestDeleteRequest(t *testing.T) {
	requests := make(chan capturedRequest, 1)
	store := newHTTPTestStore(t, 1, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests <- captureRequest(request)
		response.WriteHeader(http.StatusNoContent)
	}))
	if err := store.Delete(context.Background(), "object", 7); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	request := receiveRequest(t, requests)
	if request.method != http.MethodDelete || request.path != "/storage/v1/b/"+testBucket+"/o/object" {
		t.Errorf("request = %s %s", request.method, request.path)
	}
	if request.query.Get("ifGenerationMatch") != "7" {
		t.Errorf("query = %v, want ifGenerationMatch=7", request.query)
	}
}

func TestListRequest(t *testing.T) {
	modified := time.Date(2026, time.August, 22, 10, 11, 12, 0, time.UTC)
	requests := make(chan capturedRequest, 2)
	var attempts atomic.Int64
	items := []objectResponse{
		objectResponseFor("prefix/a", 7, []byte("a"), modified, 0),
		objectResponseFor("prefix/b", 8, []byte("bb"), modified.Add(time.Second), 0),
	}
	store := newHTTPTestStore(t, 1<<20, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempt := attempts.Add(1)
		requests <- captureRequest(request)
		response.Header().Set("Content-Type", "application/json")
		nextCursor := ""
		if attempt == 1 {
			nextCursor = "next-cursor"
		}
		payload := struct {
			Items         []objectResponse `json:"items"`
			NextPageToken string           `json:"nextPageToken"`
		}{Items: items, NextPageToken: nextCursor}
		if err := json.NewEncoder(response).Encode(payload); err != nil {
			t.Errorf("encode list response: %v", err)
		}
	}))

	cursor, err := blob.WrapCursor("prefix/", "cursor-token")
	if err != nil {
		t.Fatalf("wrap input cursor: %v", err)
	}
	result, err := store.List(context.Background(), "prefix/", cursor)
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	want := []blob.Attributes{
		{Key: "prefix/a", Generation: 7, Size: 1, Modified: modified},
		{Key: "prefix/b", Generation: 8, Size: 2, Modified: modified.Add(time.Second)},
	}
	if len(result.Objects) != len(want) {
		t.Fatalf("objects = %+v, want %+v", result.Objects, want)
	}
	for index := range want {
		if result.Objects[index] != want[index] {
			t.Errorf("object %d = %+v, want %+v", index, result.Objects[index], want[index])
		}
	}
	if result.NextCursor == "next-cursor" || result.NextCursor == "" {
		t.Errorf("next cursor is not wrapped: %q", result.NextCursor)
	}
	nextProviderCursor, err := blob.UnwrapCursor("prefix/", result.NextCursor)
	if err != nil || nextProviderCursor != "next-cursor" {
		t.Errorf("unwrapped next cursor = (%q, %v), want next-cursor", nextProviderCursor, err)
	}
	if attempts.Load() != 1 {
		t.Errorf("list attempts = %d, want one provider page", attempts.Load())
	}

	request := receiveRequest(t, requests)
	if request.method != http.MethodGet || request.path != "/storage/v1/b/"+testBucket+"/o" {
		t.Errorf("request = %s %s", request.method, request.path)
	}
	if request.query.Get("prefix") != "prefix/" || request.query.Get("pageToken") != "cursor-token" ||
		request.query.Get("maxResults") != fmt.Sprint(blob.MaxListPage) || request.query.Get("projection") != "noAcl" {
		t.Errorf("query = %v", request.query)
	}
	fields := request.query.Get("fields")
	for _, field := range []string{"nextPageToken", "name", "generation", "size", "updated"} {
		if !strings.Contains(fields, field) {
			t.Errorf("fields = %q, missing %q", fields, field)
		}
	}

	wrongPrefix, err := store.List(context.Background(), "other/", cursor)
	assertZeroListResult(t, wrongPrefix)
	assertOperationError(t, err, nil, "list", "other/", blob.ErrPrecondition)
	if attempts.Load() != 1 {
		t.Errorf("wrong-prefix cursor made a provider request; attempts = %d", attempts.Load())
	}
}

func TestReaderResumePinsGeneration(t *testing.T) {
	data := []byte("object data")
	const firstRead = 4
	const generation = 7
	modified := time.Date(2026, time.August, 22, 10, 11, 12, 0, time.UTC)
	requests := make(chan capturedRequest, 2)
	var attempts atomic.Int64
	client := newTestClient(t, "https://storage.test/storage/v1/", roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		requests <- capturedRequest{
			method: request.Method,
			path:   request.URL.Path,
			query:  request.URL.Query(),
			header: request.Header.Clone(),
		}
		header := http.Header{
			"Content-Type":      []string{contentType},
			"Last-Modified":     []string{modified.Format(http.TimeFormat)},
			"X-Goog-Generation": []string{fmt.Sprint(generation)},
			"X-Goog-Hash":       []string{"crc32c=" + encodeGCSCRC32C(crc32c(data))},
		}
		if attempt == 1 {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Status:        "200 OK",
				Header:        header,
				Body:          io.NopCloser(&dataErrorReader{data: data[:firstRead], err: io.ErrUnexpectedEOF}),
				ContentLength: int64(len(data)),
				Request:       request,
			}, nil
		}
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", firstRead, len(data)-1, len(data)))
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Status:        "206 Partial Content",
			Header:        header,
			Body:          io.NopCloser(bytes.NewReader(data[firstRead:])),
			ContentLength: int64(len(data) - firstRead),
			Request:       request,
		}, nil
	}))
	store, err := New(client, testBucket, 1<<20)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	object, err := store.Get(context.Background(), "object")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if !bytes.Equal(object.Data, data) || object.Attributes.Generation != generation {
		t.Errorf("object = %+v, want data %q generation %d", object, data, generation)
	}
	if attempts.Load() != 2 {
		t.Fatalf("reader attempts = %d, want initial request plus one body resume", attempts.Load())
	}
	initial := receiveRequest(t, requests)
	resume := receiveRequest(t, requests)
	if initial.query.Get("generation") != "" || initial.header.Get("Range") != "" {
		t.Errorf("initial request = query %v Range %q", initial.query, initial.header.Get("Range"))
	}
	if resume.query.Get("generation") != fmt.Sprint(generation) || resume.header.Get("Range") != fmt.Sprintf("bytes=%d-", firstRead) {
		t.Errorf("resume request = query %v Range %q", resume.query, resume.header.Get("Range"))
	}
}

func TestRetryNeverMakesOneSDKAttempt(t *testing.T) {
	tests := []struct {
		name      string
		op        string
		key       string
		ambiguous bool
		run       func(*testing.T, context.Context, *Store) error
	}{
		{name: "Head", op: "head", key: "object", run: func(t *testing.T, ctx context.Context, store *Store) error {
			attributes, err := store.Head(ctx, "object")
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Get", op: "get", key: "object", run: func(t *testing.T, ctx context.Context, store *Store) error {
			object, err := store.Get(ctx, "object")
			assertZeroObject(t, &object)
			return err
		}},
		{name: "Create", op: "create", key: "object", ambiguous: true, run: func(t *testing.T, ctx context.Context, store *Store) error {
			attributes, err := store.Create(ctx, "object", []byte("data"))
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Replace", op: "replace", key: "object", ambiguous: true, run: func(t *testing.T, ctx context.Context, store *Store) error {
			attributes, err := store.Replace(ctx, "object", 7, []byte("data"))
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Delete", op: "delete", key: "object", ambiguous: true, run: func(_ *testing.T, ctx context.Context, store *Store) error {
			return store.Delete(ctx, "object", 7)
		}},
		{name: "List", op: "list", key: "prefix/", run: func(t *testing.T, ctx context.Context, store *Store) error {
			result, err := store.List(ctx, "prefix/", "")
			assertZeroListResult(t, result)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &retryProbeTransport{}
			client := newTestClient(t, "https://storage.test/storage/v1/", transport)
			store, err := New(client, testBucket, 1<<20)
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = test.run(t, ctx, store)
			want := []error{blob.ErrUnavailable}
			if test.ambiguous {
				want = append(want, blob.ErrAmbiguous)
			}
			assertOperationError(t, err, nil, test.op, test.key, want...)
			if transport.attempts.Load() != 1 {
				t.Errorf("SDK attempts = %d, want 1; RetryNever did not suppress retry", transport.attempts.Load())
			}
		})
	}
}

func TestSDKTransportErrors(t *testing.T) {
	tests := []struct {
		name      string
		op        string
		cause     error
		ambiguous bool
		run       func(*testing.T, *Store) error
	}{
		{name: "Get timeout", op: "get", cause: timeoutFailure{}, run: func(t *testing.T, store *Store) error {
			object, err := store.Get(context.Background(), "object")
			assertZeroObject(t, &object)
			return err
		}},
		{name: "Head transport", op: "head", cause: errors.New("dial failure"), run: func(t *testing.T, store *Store) error {
			attributes, err := store.Head(context.Background(), "object")
			assertZeroAttributes(t, attributes)
			return err
		}},
		{name: "Create timeout", op: "create", cause: timeoutFailure{}, ambiguous: true, run: func(t *testing.T, store *Store) error {
			attributes, err := store.Create(context.Background(), "object", []byte("data"))
			assertZeroAttributes(t, attributes)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &errorTransport{err: test.cause}
			client := newTestClient(t, "https://storage.test/storage/v1/", transport)
			store, err := New(client, testBucket, 1<<20)
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			err = test.run(t, store)
			want := []error{blob.ErrUnavailable}
			if test.ambiguous {
				want = append(want, blob.ErrAmbiguous)
			}
			assertOperationError(t, err, test.cause, test.op, "object", want...)
			if transport.attempts.Load() != 1 {
				t.Errorf("transport attempts = %d, want 1", transport.attempts.Load())
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type retryProbeTransport struct {
	attempts atomic.Int64
}

func (transport *retryProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	attempt := transport.attempts.Add(1)
	if request.Body != nil {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return nil, err
		}
	}
	status := http.StatusServiceUnavailable
	if attempt > 1 {
		status = http.StatusBadRequest
	}
	return errorResponse(request, status), nil
}

type errorTransport struct {
	err      error
	attempts atomic.Int64
}

func (transport *errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.attempts.Add(1)
	return nil, transport.err
}

type timeoutFailure struct{}

func (timeoutFailure) Error() string   { return "transport timeout" }
func (timeoutFailure) Timeout() bool   { return true }
func (timeoutFailure) Temporary() bool { return true }

var _ net.Error = timeoutFailure{}

type errorReader struct {
	err error
}

func (reader errorReader) Read([]byte) (int, error) {
	return 0, reader.err
}

type dataErrorReader struct {
	data []byte
	err  error
	done bool
}

func (reader *dataErrorReader) Read(buffer []byte) (int, error) {
	if reader.done {
		return 0, reader.err
	}
	reader.done = true
	return copy(buffer, reader.data), reader.err
}

type capturedRequest struct {
	method  string
	path    string
	query   url.Values
	header  http.Header
	body    []byte
	readErr error
}

func captureRequest(request *http.Request) capturedRequest {
	body, err := io.ReadAll(request.Body)
	return capturedRequest{
		method:  request.Method,
		path:    request.URL.Path,
		query:   request.URL.Query(),
		header:  request.Header.Clone(),
		body:    body,
		readErr: err,
	}
}

func receiveRequest(t *testing.T, requests <-chan capturedRequest) capturedRequest {
	t.Helper()
	request := <-requests
	if request.readErr != nil {
		t.Fatalf("read request: %v", request.readErr)
	}
	return request
}

type objectResponse struct {
	Name        string `json:"name"`
	Generation  string `json:"generation"`
	Size        string `json:"size"`
	Updated     string `json:"updated"`
	CRC32C      string `json:"crc32c,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

func objectResponseFor(key string, generation int64, data []byte, modified time.Time, checksum uint32) objectResponse {
	response := objectResponse{
		Name:       key,
		Generation: fmt.Sprint(generation),
		Size:       fmt.Sprint(len(data)),
		Updated:    modified.Format(time.RFC3339Nano),
	}
	if checksum != 0 {
		response.CRC32C = encodeGCSCRC32C(checksum)
	}
	return response
}

func writeObjectResponse(
	t *testing.T,
	response http.ResponseWriter,
	key string,
	generation int64,
	data []byte,
	modified time.Time,
	checksum *uint32,
) {
	t.Helper()
	object := objectResponseFor(key, generation, data, modified, 0)
	if checksum != nil {
		object.CRC32C = encodeGCSCRC32C(*checksum)
	}
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(object); err != nil {
		t.Errorf("encode object response: %v", err)
	}
}

func writeMediaResponse(
	t *testing.T,
	response http.ResponseWriter,
	data []byte,
	generation int64,
	modified time.Time,
	checksum *uint32,
) {
	t.Helper()
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("X-Goog-Generation", fmt.Sprint(generation))
	response.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
	if checksum != nil {
		response.Header().Set("X-Goog-Hash", "crc32c="+encodeGCSCRC32C(*checksum))
	}
	if _, err := response.Write(data); err != nil {
		t.Errorf("write media response: %v", err)
	}
}

type uploadMetadata struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	CRC32C      string `json:"crc32c"`
}

func parseMultipartUpload(t *testing.T, request *capturedRequest) (metadata uploadMetadata, data []byte) {
	t.Helper()
	mediaType, parameters, err := mime.ParseMediaType(request.header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse upload content type: %v", err)
	}
	if mediaType != "multipart/related" {
		t.Fatalf("upload content type = %q", mediaType)
	}
	reader := multipart.NewReader(bytes.NewReader(request.body), parameters["boundary"])
	metadataPart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("read metadata part: %v", err)
	}
	if err := json.NewDecoder(metadataPart).Decode(&metadata); err != nil {
		t.Fatalf("decode upload metadata: %v", err)
	}
	if err := metadataPart.Close(); err != nil {
		t.Errorf("close metadata part: %v", err)
	}
	dataPart, err := reader.NextPart()
	if err != nil {
		t.Fatalf("read data part: %v", err)
	}
	data, err = io.ReadAll(dataPart)
	if err != nil {
		t.Fatalf("read upload data: %v", err)
	}
	if err := dataPart.Close(); err != nil {
		t.Errorf("close data part: %v", err)
	}
	if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
		t.Errorf("multipart trailer error = %v, want EOF", err)
	}
	return metadata, data
}

func newHTTPTestStore(t *testing.T, maximum int64, handler http.Handler) *Store {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := newTestClient(t, server.URL+"/storage/v1/", server.Client().Transport)
	store, err := New(client, testBucket, maximum)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return store
}

func newTestClient(t *testing.T, endpoint string, transport http.RoundTripper) *storage.Client {
	t.Helper()
	client, err := storage.NewClient(
		context.Background(),
		storage.WithJSONReads(),
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatalf("storage.NewClient(): %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close storage client: %v", err)
		}
	})
	return client
}

func errorResponse(request *http.Request, status int) *http.Response {
	body := fmt.Sprintf(`{"error":{"code":%d,"message":"test error"}}`, status)
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func apiError(status int) error {
	return &googleapi.Error{Code: status, Message: http.StatusText(status)}
}

func gcsCRC32C(data []byte) *uint32 {
	return new(crc32c(data))
}

func encodeGCSCRC32C(checksum uint32) string {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], checksum)
	return base64.StdEncoding.EncodeToString(encoded[:])
}

func assertOperationError(t *testing.T, err, cause error, op, key string, want ...error) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil")
	}
	var operation *blob.OpError
	if !errors.As(err, &operation) {
		t.Fatalf("error type = %T, want *blob.OpError", err)
	}
	if operation.Op != op || operation.Key != key {
		t.Errorf("operation error = %+v, want op %q key %q", operation, op, key)
	}
	for _, category := range blobCategories {
		wantCategory := slices.Contains(want, category)
		if errors.Is(err, category) != wantCategory {
			t.Errorf("errors.Is(%v, %v) = %t, want %t", err, category, errors.Is(err, category), wantCategory)
		}
	}
	for _, target := range want {
		if !errors.Is(err, target) {
			t.Errorf("error = %v, want %v", err, target)
		}
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("error = %v, want cause %v", err, cause)
	}
}

func assertZeroObject(t *testing.T, object *blob.Object) {
	t.Helper()
	if object.Data != nil || object.Attributes != (blob.Attributes{}) {
		t.Errorf("object = %+v, want zero", object)
	}
}

func assertZeroAttributes(t *testing.T, attributes blob.Attributes) {
	t.Helper()
	if attributes != (blob.Attributes{}) {
		t.Errorf("attributes = %+v, want zero", attributes)
	}
}

func assertZeroListResult(t *testing.T, result blob.ListResult) {
	t.Helper()
	if result.Objects != nil || result.NextCursor != "" {
		t.Errorf("result = %+v, want zero", result)
	}
}
