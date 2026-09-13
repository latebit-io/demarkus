package fetch

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
)

// ErrResponseBudget identifies exhausted caller-wide response reads, including retries.
var ErrResponseBudget = errors.New("response byte budget exhausted")

type responseBudgetKey struct{}

// ResponseBudget bounds bytes read across concurrent requests sharing a context.
type ResponseBudget struct {
	remaining atomic.Int64
	read      atomic.Int64
}

// WithResponseBudget shares a response-read limit across child requests.
func WithResponseBudget(ctx context.Context, bytes int64) (context.Context, *ResponseBudget) {
	b := &ResponseBudget{}
	b.remaining.Store(max(bytes, 0))
	return context.WithValue(ctx, responseBudgetKey{}, b), b
}

// BytesRead includes failed and retried response reads.
func (b *ResponseBudget) BytesRead() int64 { return b.read.Load() }

type budgetReader struct {
	reader io.Reader
	budget *ResponseBudget
}

func (r budgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var reserved int64
	for {
		remaining := r.budget.remaining.Load()
		if remaining == 0 {
			return 0, ErrResponseBudget
		}
		reserved = min(remaining, int64(len(p)))
		if r.budget.remaining.CompareAndSwap(remaining, remaining-reserved) {
			break
		}
	}
	// Reserve before I/O; outstanding reads may conservatively exhaust the budget.
	n, err := r.reader.Read(p[:reserved])
	r.budget.remaining.Add(reserved - int64(n))
	r.budget.read.Add(int64(n))
	return n, err
}

func responseReader(ctx context.Context, reader io.Reader) io.Reader {
	if budget, ok := ctx.Value(responseBudgetKey{}).(*ResponseBudget); ok {
		return budgetReader{reader: reader, budget: budget}
	}
	return reader
}
