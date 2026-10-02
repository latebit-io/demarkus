package oauthsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/latebit-io/demarkus/server/blob"
)

var (
	// errRecordExists is a create of a key already held.
	errRecordExists = errors.New("record exists")
	// errRecordChanged is a replace that lost to a concurrent write.
	errRecordChanged = errors.New("record changed")
)

// stateDir is one prefix of the state bucket holding a JSON record per key.
type stateDir struct {
	objects blob.Store
	prefix  string
}

// create stores a new record; a key already held is errRecordExists.
func (d stateDir) create(ctx context.Context, name string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode %s%s: %w", d.prefix, name, err)
	}
	_, err = d.objects.Create(ctx, d.prefix+name, data)
	if errors.Is(err, blob.ErrPrecondition) {
		return errRecordExists
	}
	if err != nil {
		return fmt.Errorf("store %s%s: %w", d.prefix, name, err)
	}
	return nil
}

// get decodes the record under name into out and returns its generation;
// an absent record is blob.ErrNotFound.
func (d stateDir) get(ctx context.Context, name string, out any) (blob.Generation, error) {
	object, err := d.objects.Get(ctx, d.prefix+name)
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(object.Data, out); err != nil {
		return 0, fmt.Errorf("decode %s%s: %w", d.prefix, name, err)
	}
	return object.Attributes.Generation, nil
}

// replace overwrites name if it is still at generation; a concurrent write
// or delete is errRecordChanged.
func (d stateDir) replace(ctx context.Context, name string, generation blob.Generation, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode %s%s: %w", d.prefix, name, err)
	}
	_, err = d.objects.Replace(ctx, d.prefix+name, generation, data)
	if errors.Is(err, blob.ErrPrecondition) || errors.Is(err, blob.ErrNotFound) {
		return errRecordChanged
	}
	if err != nil {
		return fmt.Errorf("store %s%s: %w", d.prefix, name, err)
	}
	return nil
}

// list returns the attributes of every record under sub, a prefix within
// the directory.
func (d stateDir) list(ctx context.Context, sub string) ([]blob.Attributes, error) {
	var out []blob.Attributes
	err := d.each(ctx, sub, func(attrs blob.Attributes) error {
		out = append(out, attrs)
		return nil
	})
	return out, err
}

// each calls visit for every object under sub, a page at a time.
func (d stateDir) each(ctx context.Context, sub string, visit func(blob.Attributes) error) error {
	cursor := ""
	for {
		page, err := d.objects.List(ctx, d.prefix+sub, "", cursor)
		if err != nil {
			return fmt.Errorf("list %s%s: %w", d.prefix, sub, err)
		}
		for _, attrs := range page.Objects {
			if err := visit(attrs); err != nil {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// remove deletes name at its current generation; absent is success, and a
// concurrent rewrite is retried.
func (d stateDir) remove(ctx context.Context, name string) error {
	for range 3 {
		attrs, err := d.objects.Head(ctx, d.prefix+name)
		if errors.Is(err, blob.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("delete %s%s: %w", d.prefix, name, err)
		}
		if done, err := d.removeAt(ctx, attrs.Key, attrs.Generation); done || err != nil {
			return err
		}
	}
	return fmt.Errorf("delete %s%s: changed on every attempt", d.prefix, name)
}

// removeAt deletes key at generation; done is false when it changed since.
func (d stateDir) removeAt(ctx context.Context, key string, generation blob.Generation) (done bool, err error) {
	err = d.objects.Delete(ctx, key, generation)
	switch {
	case err == nil, errors.Is(err, blob.ErrNotFound):
		return true, nil
	case errors.Is(err, blob.ErrPrecondition):
		return false, nil
	default:
		return false, fmt.Errorf("delete %s: %w", key, err)
	}
}

// sweep deletes every record expired reports as expired, at the generation
// it returns; a record changed meanwhile waits for the next pass.
func (d stateDir) sweep(ctx context.Context, expired func(blob.Attributes) (bool, blob.Generation, error)) (int, error) {
	swept := 0
	err := d.each(ctx, "", func(attrs blob.Attributes) error {
		gone, generation, err := expired(attrs)
		if errors.Is(err, blob.ErrNotFound) || err == nil && !gone {
			return nil
		}
		if err != nil {
			return err
		}
		done, err := d.removeAt(ctx, attrs.Key, generation)
		if done {
			swept++
		}
		return err
	})
	return swept, err
}
