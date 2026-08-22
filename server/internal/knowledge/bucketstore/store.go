package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/knowledge/blob"
)

const (
	defaultRequestTimeout = 10 * time.Second
	defaultShardWorkers   = 16
	defaultCommitInterval = 1500 * time.Millisecond
)

// Options configures one world store.
type Options struct {
	WorldID        string
	RequestTimeout time.Duration
	ShardWorkers   int
	RequirePolicy  bool
}

// Store owns a validated immutable snapshot for one world.
type Store struct {
	objects        blob.Store
	worldID        string
	requestTimeout time.Duration
	shardWorkers   int
	requirePolicy  bool
	snapshot       atomic.Pointer[snapshot]
	refreshMu      sync.Mutex
	commitToken    chan struct{}
	commitInterval time.Duration
	lastHeadTry    time.Time
	now            func() time.Time
	newOperationID func() (string, error)
}

var (
	_ backend.Reader        = (*Store)(nil)
	_ backend.CatalogReader = (*Store)(nil)
	_ backend.ViewProvider  = (*Store)(nil)
)

type snapshotEntry struct {
	PathHash string
	Manifest objectRef
	Current  int
	Archived bool
	BodyHash string
	Modified time.Time
}

type directoryChild struct {
	Name    string
	IsDir   bool
	Visible bool
	Live    bool
}

type directoryNode struct {
	Children []directoryChild
}

type snapshot struct {
	Head           headObject
	HeadAttributes blob.Attributes
	HeadGeneration blob.Generation
	Root           rootObject
	Shards         *[shardCount]shardObject
	Paths          map[string]snapshotEntry
	BodyHashes     map[string]string
	Directories    map[string]directoryNode
	Catalog        *catalog.Catalog
}

// Open loads and atomically installs a fully validated root snapshot.
func Open(ctx context.Context, objects blob.Store, options Options) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open bucket store: %w: context is nil", blob.ErrPrecondition)
	}
	if nilStore(objects) {
		return nil, fmt.Errorf("open bucket store: %w: blob store is nil", blob.ErrPrecondition)
	}
	if !validWorldID(options.WorldID) {
		return nil, fmt.Errorf("open bucket store: %w: invalid world ID %q", blob.ErrPrecondition, options.WorldID)
	}
	if options.RequestTimeout < 0 {
		return nil, fmt.Errorf("open bucket store: %w: request timeout must not be negative", blob.ErrPrecondition)
	}
	if options.ShardWorkers < 0 {
		return nil, fmt.Errorf("open bucket store: %w: shard workers must not be negative", blob.ErrPrecondition)
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.ShardWorkers == 0 {
		options.ShardWorkers = defaultShardWorkers
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open bucket store: %w", err)
	}

	store := &Store{
		objects:        objects,
		worldID:        options.WorldID,
		requestTimeout: options.RequestTimeout,
		shardWorkers:   options.ShardWorkers,
		requirePolicy:  options.RequirePolicy,
		commitToken:    make(chan struct{}, 1),
		commitInterval: defaultCommitInterval,
		now:            time.Now,
		newOperationID: randomOperationID,
	}
	store.commitToken <- struct{}{}
	requestCtx, cancel := context.WithTimeout(ctx, store.requestTimeout)
	defer cancel()
	store.refreshMu.Lock()
	loaded, err := loadRootSnapshot(requestCtx, store.objects, store.worldID, store.shardWorkers)
	if err != nil {
		store.refreshMu.Unlock()
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	store.snapshot.Store(loaded)
	store.refreshMu.Unlock()
	return store, nil
}

func loadRootSnapshot(ctx context.Context, objects blob.Store, worldID string, workers int) (*snapshot, error) {
	head, headAttributes, err := loadHeadObject(ctx, objects, worldID)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("head is not initialized: %w", err)
		}
		return nil, err
	}

	root, err := getImmutable(ctx, objects, head.Root, rootKey(head.Root.Hash), func(root *rootObject) error {
		return validateRootObject(root, head.WorldID)
	})
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := loadShards(ctx, objects, root.Shards, nil, workers)
	if err != nil {
		return nil, err
	}
	loaded, err := buildDerivedSnapshot(&head, headAttributes, &root, shards)
	if err != nil {
		return nil, fmt.Errorf("%w: build snapshot: %v", blob.ErrIntegrity, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return loaded, nil
}

func loadHeadObject(ctx context.Context, objects blob.Store, worldID string) (headObject, blob.Attributes, error) {
	var head headObject
	headValue, err := objects.Get(ctx, headObjectKey)
	if err != nil {
		return head, blob.Attributes{}, fmt.Errorf("read head: %w", err)
	}
	if err := validateReadObject(headObjectKey, &headValue); err != nil {
		return head, blob.Attributes{}, err
	}
	if err := decodeImmutable(headValue.Data, &head); err != nil {
		return head, blob.Attributes{}, fmt.Errorf("%w: decode head: %v", blob.ErrIntegrity, err)
	}
	if err := validateHeadObject(&head); err != nil {
		return head, blob.Attributes{}, fmt.Errorf("%w: validate head: %v", blob.ErrIntegrity, err)
	}
	if head.WorldID != worldID {
		return head, blob.Attributes{}, fmt.Errorf("%w: configured world ID %q does not match head world ID %q", blob.ErrPrecondition, worldID, head.WorldID)
	}
	return head, headValue.Attributes, nil
}

func (store *Store) refreshSnapshot(ctx context.Context) (*snapshot, error) {
	headAttributes, err := store.objects.Head(ctx, headObjectKey)
	if err != nil {
		return nil, fmt.Errorf("head snapshot: %w", err)
	}
	if err := validateHeadOnly(headAttributes); err != nil {
		return nil, err
	}
	cached := store.snapshot.Load()
	if cached != nil && headAttributes.Generation == cached.HeadGeneration {
		if err := validateCachedHead(cached, headAttributes); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cached, nil
	}

	store.refreshMu.Lock()
	defer store.refreshMu.Unlock()
	cached = store.snapshot.Load()
	if cached != nil && headAttributes.Generation == cached.HeadGeneration {
		if err := validateCachedHead(cached, headAttributes); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cached, nil
	}

	head, currentAttributes, err := loadHeadObject(ctx, store.objects, store.worldID)
	if err != nil {
		return nil, err
	}
	if cached != nil && head.Sequence <= cached.Head.Sequence {
		return nil, fmt.Errorf("%w: head sequence moved from %d to %d", blob.ErrIntegrity, cached.Head.Sequence, head.Sequence)
	}
	root, err := getImmutable(ctx, store.objects, head.Root, rootKey(head.Root.Hash), func(root *rootObject) error {
		return validateRootObject(root, head.WorldID)
	})
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := loadShards(ctx, store.objects, root.Shards, cached, store.shardWorkers)
	if err != nil {
		return nil, err
	}
	loaded, err := buildDerivedSnapshot(&head, currentAttributes, &root, shards)
	if err != nil {
		return nil, fmt.Errorf("%w: build snapshot: %v", blob.ErrIntegrity, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.snapshot.Store(loaded)
	return loaded, nil
}

func validateHeadOnly(attributes blob.Attributes) error {
	if attributes.Key != headObjectKey || attributes.Generation <= 0 || attributes.Size <= 0 || attributes.Modified.IsZero() {
		return fmt.Errorf("%w: object %q has inconsistent attributes", blob.ErrIntegrity, headObjectKey)
	}
	return nil
}

func validateCachedHead(cached *snapshot, attributes blob.Attributes) error {
	want := cached.HeadAttributes
	if attributes.Key != want.Key || attributes.Size != want.Size || !attributes.Modified.Equal(want.Modified) {
		return fmt.Errorf("%w: object %q changed attributes without changing generation", blob.ErrIntegrity, headObjectKey)
	}
	return nil
}

func loadShards(
	ctx context.Context,
	objects blob.Store,
	refs []shardRef,
	previous *snapshot,
	workers int,
) (*[shardCount]shardObject, error) {
	loaded := new([shardCount]shardObject)
	if workers < 1 {
		return nil, fmt.Errorf("%w: shard workers must be positive", blob.ErrPrecondition)
	}
	changed := make([]int, 0, shardCount)
	for index := range shardCount {
		if previous != nil && refs[index] == previous.Root.Shards[index] {
			loaded[index] = previous.Shards[index]
			continue
		}
		changed = append(changed, index)
	}
	if len(changed) == 0 {
		return loaded, nil
	}

	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int, len(changed))
	for _, index := range changed {
		jobs <- index
	}
	close(jobs)

	var wait sync.WaitGroup
	for range min(workers, len(changed)) {
		wait.Go(func() {
			for index := range jobs {
				if workCtx.Err() != nil {
					return
				}
				ref := refs[index]
				expectedShard := fmt.Sprintf("%02x", index)
				shard, err := getImmutable(workCtx, objects, ref.objectRef, shardKey(expectedShard, ref.Hash), func(shard *shardObject) error {
					return validateShardObject(shard, expectedShard)
				})
				if err != nil {
					cancel(fmt.Errorf("load shard %s: %w", expectedShard, err))
					return
				}
				loaded[index] = shard
			}
		})
	}
	wait.Wait()
	if cause := context.Cause(workCtx); cause != nil {
		return nil, cause
	}
	return loaded, nil
}

func getImmutable[T any](
	ctx context.Context,
	objects blob.Store,
	ref objectRef,
	expectedKey string,
	validate func(*T) error,
) (T, error) {
	var result T
	if err := verifyRef(ref, expectedKey); err != nil {
		return result, fmt.Errorf("%w: %v", blob.ErrIntegrity, err)
	}
	value, err := objects.Get(ctx, ref.Key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return result, fmt.Errorf("%w: referenced object %q is missing: %w", blob.ErrIntegrity, ref.Key, err)
		}
		return result, fmt.Errorf("read referenced object %q: %w", ref.Key, err)
	}
	if err := validateReadObject(ref.Key, &value); err != nil {
		return result, err
	}
	if actual := hashHex(value.Data); actual != ref.Hash {
		return result, fmt.Errorf("%w: object %q hash is %s, reference is %s", blob.ErrIntegrity, ref.Key, actual, ref.Hash)
	}
	if err := decodeImmutable(value.Data, &result); err != nil {
		return result, fmt.Errorf("%w: decode object %q: %v", blob.ErrIntegrity, ref.Key, err)
	}
	if err := validate(&result); err != nil {
		return result, fmt.Errorf("%w: validate object %q: %v", blob.ErrIntegrity, ref.Key, err)
	}
	return result, nil
}

func validateReadObject(key string, object *blob.Object) error {
	attributes := object.Attributes
	if attributes.Key != key || attributes.Generation <= 0 || attributes.Size != int64(len(object.Data)) || attributes.Modified.IsZero() {
		return fmt.Errorf("%w: object %q has inconsistent attributes", blob.ErrIntegrity, key)
	}
	return nil
}

func buildDerivedSnapshot(
	head *headObject,
	headAttributes blob.Attributes,
	root *rootObject,
	shards *[shardCount]shardObject,
) (*snapshot, error) {
	loaded := &snapshot{
		Head:           *head,
		HeadAttributes: headAttributes,
		HeadGeneration: headAttributes.Generation,
		Root:           *root,
		Shards:         shards,
		Paths:          make(map[string]snapshotEntry, root.DocumentCount),
		BodyHashes:     make(map[string]string, root.DocumentCount),
		Directories:    make(map[string]directoryNode),
		Catalog:        catalog.New(),
	}
	for shardIndex := range shardCount {
		for entryIndex := range shards[shardIndex].Entries {
			entry := &shards[shardIndex].Entries[entryIndex]
			if _, exists := loaded.Paths[entry.Path]; exists {
				return nil, fmt.Errorf("duplicate path %q", entry.Path)
			}
			modified, err := parseTimestamp(entry.Modified)
			if err != nil {
				return nil, fmt.Errorf("path %q modified: %w", entry.Path, err)
			}
			loaded.Paths[entry.Path] = snapshotEntry{
				PathHash: entry.PathHash,
				Manifest: entry.Manifest,
				Current:  entry.Current,
				Archived: entry.Archived,
				BodyHash: entry.BodyHash,
				Modified: modified,
			}
			if entry.Archived {
				continue
			}
			if previous, exists := loaded.BodyHashes[entry.BodyHash]; !exists || entry.Path < previous {
				loaded.BodyHashes[entry.BodyHash] = entry.Path
			}
			importance, err := parseCanonicalImportance(entry.Catalog.Importance)
			if err != nil {
				return nil, fmt.Errorf("path %q catalog importance: %w", entry.Path, err)
			}
			loaded.Catalog.Set(&catalog.Entry{
				Path:       entry.Path,
				Tags:       slices.Clone(entry.Catalog.Tags),
				Importance: importance,
				Title:      entry.Catalog.Title,
				Modified:   modified,
				Metadata:   maps.Clone(entry.Catalog.Metadata),
			})
		}
	}
	if len(loaded.Paths) != root.DocumentCount {
		return nil, fmt.Errorf("root document count is %d, loaded %d paths", root.DocumentCount, len(loaded.Paths))
	}
	if err := validatePathTopology(loaded.Paths); err != nil {
		return nil, err
	}
	loaded.Directories = buildDirectories(loaded.Paths)
	return loaded, nil
}

func validatePathTopology(paths map[string]snapshotEntry) error {
	for path := range paths {
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		ancestor := ""
		for _, part := range parts[:len(parts)-1] {
			ancestor += "/" + part
			if _, exists := paths[ancestor]; exists {
				return fmt.Errorf("document %q has document ancestor %q", path, ancestor)
			}
		}
	}
	return nil
}

func buildDirectories(paths map[string]snapshotEntry) map[string]directoryNode {
	mutable := map[string]map[string]directoryChild{"/": {}}
	orderedPaths := slices.Collect(maps.Keys(paths))
	sort.Strings(orderedPaths)
	for _, path := range orderedPaths {
		entry := paths[path]
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		lastHidden := -1
		for index, name := range parts {
			if hiddenLogicalName(name) {
				lastHidden = index
			}
		}
		parent := "/"
		for index, name := range parts {
			isDirectory := index < len(parts)-1
			visible := index > lastHidden
			child := mutable[parent][name]
			child.Name = name
			child.IsDir = isDirectory
			child.Visible = child.Visible || visible
			child.Live = child.Live || visible && !entry.Archived
			mutable[parent][name] = child
			if !isDirectory {
				continue
			}
			parent = joinPath(parent, name)
			if mutable[parent] == nil {
				mutable[parent] = make(map[string]directoryChild)
			}
		}
	}
	directories := make(map[string]directoryNode, len(mutable))
	for path, children := range mutable {
		ordered := make([]directoryChild, 0, len(children))
		for _, child := range children {
			ordered = append(ordered, child)
		}
		sort.Slice(ordered, func(left, right int) bool {
			return ordered[left].Name < ordered[right].Name
		})
		directories[path] = directoryNode{Children: ordered}
	}
	return directories
}

func hiddenLogicalName(name string) bool {
	return strings.HasPrefix(name, ".") || name == "versions"
}

func joinPath(parent, child string) string {
	if parent == "/" {
		return parent + child
	}
	return parent + "/" + child
}

func nilStore(objects blob.Store) bool {
	if objects == nil {
		return true
	}
	value := reflect.ValueOf(objects)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
