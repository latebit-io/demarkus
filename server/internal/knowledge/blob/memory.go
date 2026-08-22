package blob

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxGeneration = Generation(1<<63 - 1)
)

type memoryObject struct {
	data       []byte
	attributes Attributes
}

// Memory is a concurrency-safe in-memory Store.
type Memory struct {
	mu             sync.RWMutex
	maxObjectBytes int64
	lastGeneration Generation
	objects        map[string]memoryObject
}

// NewMemory returns an empty store with a positive object-size limit.
func NewMemory(maxObjectBytes int64) (*Memory, error) {
	if maxObjectBytes <= 0 {
		return nil, fmt.Errorf("new memory: %w: max object bytes must be positive", ErrPrecondition)
	}
	return &Memory{
		maxObjectBytes: maxObjectBytes,
		objects:        make(map[string]memoryObject),
	}, nil
}

// Get returns a copied object at its current generation.
func (m *Memory) Get(ctx context.Context, key string) (Object, error) {
	const op = "get"
	if err := operationContext(ctx, op, key); err != nil {
		return Object{}, err
	}
	if err := ValidateKey(key); err != nil {
		return Object{}, operationError(op, key, err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := operationContext(ctx, op, key); err != nil {
		return Object{}, err
	}
	object, ok := m.objects[key]
	if !ok {
		return Object{}, operationError(op, key, ErrNotFound)
	}
	if err := m.validateStoredObject(key, &object); err != nil {
		return Object{}, operationError(op, key, err)
	}
	return Object{Data: bytes.Clone(object.data), Attributes: object.attributes}, nil
}

// Head returns attributes for the current object generation.
func (m *Memory) Head(ctx context.Context, key string) (Attributes, error) {
	const op = "head"
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	if err := ValidateKey(key); err != nil {
		return Attributes{}, operationError(op, key, err)
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	object, ok := m.objects[key]
	if !ok {
		return Attributes{}, operationError(op, key, ErrNotFound)
	}
	if err := m.validateStoredObject(key, &object); err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	return object.attributes, nil
}

// Create stores a new object if its key does not exist.
func (m *Memory) Create(ctx context.Context, key string, data []byte) (Attributes, error) {
	const op = "create"
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	if err := ValidateKey(key); err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	if err := m.validateWriteSize(data); err != nil {
		return Attributes{}, operationError(op, key, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	if _, ok := m.objects[key]; ok {
		return Attributes{}, operationError(op, key, ErrPrecondition)
	}
	attributes, err := m.nextAttributes(key, int64(len(data)))
	if err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	m.objects[key] = memoryObject{data: bytes.Clone(data), attributes: attributes}
	return attributes, nil
}

// Replace stores a new revision when generation matches exactly.
func (m *Memory) Replace(ctx context.Context, key string, generation Generation, data []byte) (Attributes, error) {
	const op = "replace"
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	if err := ValidateKey(key); err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	if err := ValidateGeneration(generation); err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	if err := m.validateWriteSize(data); err != nil {
		return Attributes{}, operationError(op, key, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := operationContext(ctx, op, key); err != nil {
		return Attributes{}, err
	}
	current, ok := m.objects[key]
	if !ok || current.attributes.Generation != generation {
		return Attributes{}, operationError(op, key, ErrPrecondition)
	}
	attributes, err := m.nextAttributes(key, int64(len(data)))
	if err != nil {
		return Attributes{}, operationError(op, key, err)
	}
	m.objects[key] = memoryObject{data: bytes.Clone(data), attributes: attributes}
	return attributes, nil
}

// Delete removes an object when generation matches exactly.
func (m *Memory) Delete(ctx context.Context, key string, generation Generation) error {
	const op = "delete"
	if err := operationContext(ctx, op, key); err != nil {
		return err
	}
	if err := ValidateKey(key); err != nil {
		return operationError(op, key, err)
	}
	if err := ValidateGeneration(generation); err != nil {
		return operationError(op, key, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := operationContext(ctx, op, key); err != nil {
		return err
	}
	current, ok := m.objects[key]
	if !ok || current.attributes.Generation != generation {
		return operationError(op, key, ErrPrecondition)
	}
	delete(m.objects, key)
	return nil
}

// List returns up to MaxListPage current objects in key order.
func (m *Memory) List(ctx context.Context, prefix, cursor string) (ListResult, error) {
	const op = "list"
	if err := operationContext(ctx, op, prefix); err != nil {
		return ListResult{}, err
	}
	if err := ValidatePrefix(prefix); err != nil {
		return ListResult{}, operationError(op, prefix, err)
	}
	after, err := UnwrapCursor(prefix, cursor)
	if err != nil {
		return ListResult{}, operationError(op, prefix, err)
	}
	if after != "" {
		if err := ValidateKey(after); err != nil || !strings.HasPrefix(after, prefix) {
			return ListResult{}, operationError(op, prefix, fmt.Errorf("%w: invalid list cursor", ErrPrecondition))
		}
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := operationContext(ctx, op, prefix); err != nil {
		return ListResult{}, err
	}
	keys := make([]string, 0, len(m.objects))
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	start := sort.Search(len(keys), func(index int) bool {
		return keys[index] > after
	})
	end := min(start+MaxListPage, len(keys))
	result := ListResult{}
	if end > start {
		result.Objects = make([]Attributes, end-start)
		for index, key := range keys[start:end] {
			object := m.objects[key]
			if err := m.validateStoredObject(key, &object); err != nil {
				return ListResult{}, operationError(op, prefix, err)
			}
			result.Objects[index] = object.attributes
		}
	}
	if end < len(keys) {
		result.NextCursor, err = WrapCursor(prefix, keys[end-1])
		if err != nil {
			return ListResult{}, operationError(op, prefix, fmt.Errorf("%w: encode list cursor: %v", ErrIntegrity, err))
		}
	}
	return result, nil
}

func (m *Memory) validateWriteSize(data []byte) error {
	if int64(len(data)) > m.maxObjectBytes {
		return fmt.Errorf("%w: object is %d bytes, limit is %d", ErrPrecondition, len(data), m.maxObjectBytes)
	}
	return nil
}

func (m *Memory) validateStoredObject(key string, object *memoryObject) error {
	size := int64(len(object.data))
	if size > m.maxObjectBytes {
		return fmt.Errorf("%w: object is %d bytes, limit is %d", ErrIntegrity, size, m.maxObjectBytes)
	}
	if object.attributes.Key != key || object.attributes.Generation <= 0 || object.attributes.Size != size {
		return fmt.Errorf("%w: inconsistent attributes", ErrIntegrity)
	}
	return nil
}

func (m *Memory) nextAttributes(key string, size int64) (Attributes, error) {
	if m.lastGeneration == maxGeneration {
		return Attributes{}, fmt.Errorf("%w: generation space exhausted", ErrUnavailable)
	}
	m.lastGeneration++
	// Memory Modified is generation-derived logical time; its 1970 timestamp is synthetic.
	return Attributes{
		Key:        key,
		Generation: m.lastGeneration,
		Size:       size,
		Modified:   time.Unix(int64(m.lastGeneration), 0).UTC(),
	}, nil
}

func operationContext(ctx context.Context, op, key string) error {
	if err := ctx.Err(); err != nil {
		return operationError(op, key, err)
	}
	return nil
}

func operationError(op, key string, err error) error {
	return &OpError{Op: op, Key: key, Err: err}
}

var _ Store = (*Memory)(nil)
