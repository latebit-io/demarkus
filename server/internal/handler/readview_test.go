package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	storagebackend "github.com/latebit-io/demarkus/server/internal/backend"
)

type trackingViewProvider struct {
	DocumentStore
	openErr  error
	closeErr error
	opens    int
	closes   int
}

func (p *trackingViewProvider) OpenReadView(ctx context.Context) (storagebackend.ReadView, error) {
	p.opens++
	if p.openErr != nil {
		return nil, p.openErr
	}
	view, err := p.DocumentStore.OpenReadView(ctx)
	if err != nil {
		return nil, err
	}
	return &trackingReadView{ReadView: view, provider: p}, nil
}

type trackingReadView struct {
	storagebackend.ReadView
	provider *trackingViewProvider
}

func (v *trackingReadView) Close() error {
	v.provider.closes++
	if err := v.ReadView.Close(); err != nil {
		return err
	}
	return v.provider.closeErr
}

func TestHandleReadViewLifecycle(t *testing.T) {
	b := fileBackend(t)
	seedBackend(t, b, map[string]string{"hello.md": "# Hello\n"})
	provider := &trackingViewProvider{DocumentStore: b.Store}
	h := newHandler(b, nil)
	h.Store = provider

	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(context.Background(), stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusOK {
		t.Errorf("status = %q, want ok", response.Status)
	}
	if provider.opens != 1 || provider.closes != 1 {
		t.Errorf("view lifecycle = %d opens, %d closes; want 1, 1", provider.opens, provider.closes)
	}
}

func TestHandleReadViewOpenError(t *testing.T) {
	b := fileBackend(t)
	provider := &trackingViewProvider{DocumentStore: b.Store, openErr: errors.New("snapshot unavailable")}
	h := newHandler(b, nil)
	h.Store = provider

	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(context.Background(), stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusServerError {
		t.Errorf("status = %q, want server-error", response.Status)
	}
	if provider.opens != 1 || provider.closes != 0 {
		t.Errorf("view lifecycle = %d opens, %d closes; want 1, 0", provider.opens, provider.closes)
	}
}

func TestHandleReadViewCloseErrorDoesNotReplaceResponse(t *testing.T) {
	b := fileBackend(t)
	seedBackend(t, b, map[string]string{"hello.md": "# Hello\n"})
	provider := &trackingViewProvider{DocumentStore: b.Store, closeErr: errors.New("close failed")}
	h := newHandler(b, nil)
	h.Store = provider

	stream := newMockStream("FETCH /hello.md\n")
	h.HandleStream(context.Background(), stream)
	response, err := protocol.ParseResponse(&stream.output)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if response.Status != protocol.StatusOK {
		t.Errorf("status = %q, want ok", response.Status)
	}
	if provider.opens != 1 || provider.closes != 1 {
		t.Errorf("view lifecycle = %d opens, %d closes; want 1, 1", provider.opens, provider.closes)
	}
}
