package bucketstore

import (
	"context"
	"testing"
	"time"
)

func TestBoundedBy(t *testing.T) {
	limit := time.Now().Add(time.Minute)
	tests := []struct {
		name   string
		parent func() (context.Context, context.CancelFunc)
		want   time.Time
	}{
		{name: "no deadline takes the limit", parent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		}, want: limit},
		{name: "looser deadline takes the limit", parent: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), limit.Add(time.Hour))
		}, want: limit},
		{name: "tighter deadline is kept", parent: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), limit.Add(-time.Second))
		}, want: limit.Add(-time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, cancelParent := tt.parent()
			defer cancelParent()
			ctx, cancel := boundedBy(parent, limit)
			defer cancel()
			got, ok := ctx.Deadline()
			if !ok || !got.Equal(tt.want) {
				t.Errorf("deadline = %v (%v), want %v", got, ok, tt.want)
			}
		})
	}
}
