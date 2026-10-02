package bucketstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/latebit-io/demarkus/server/blob"
	"github.com/latebit-io/demarkus/server/internal/backend"
	"github.com/latebit-io/demarkus/server/internal/catalog"
	"github.com/latebit-io/demarkus/server/internal/changefeed"
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
	// ReadOnly makes every write answer backend.ErrReadOnly before any I/O,
	// and Open fails on an empty bucket instead of creating the world.
	ReadOnly bool
	// MaxDocuments caps distinct document paths (0 = unlimited); a new
	// path beyond the cap is rejected. Approximate under concurrency:
	// a per-tenant quota, not an exact invariant.
	MaxDocuments int
	// Logger receives section-index warnings; nil uses slog.Default.
	Logger *slog.Logger
	// ChangeRing enables WATCH: the hub keeps this many events from the head's
	// receipts, under the head sequence, and the store follows peers until
	// Close. Zero leaves WATCH off.
	ChangeRing int
	// Committed runs after each of this replica's own commits with its head
	// sequence, outside every lock; nil for none.
	Committed func(sequence int64)

	// followInterval overrides the backstop poll's period in tests.
	followInterval time.Duration
}

// Store owns a validated immutable snapshot for one world.
type Store struct {
	objects        blob.Store
	worldID        string
	requestTimeout time.Duration
	shardWorkers   int
	logger         *slog.Logger
	maxDocuments   int
	readOnly       bool
	snapshot       atomic.Pointer[snapshot]
	refreshMu      sync.Mutex
	commitToken    chan struct{}
	commitInterval time.Duration
	lastHeadTry    time.Time
	now            func() time.Time
	newOperationID func() (string, error)

	changes   *changefeed.Hub
	backlog   *changeLog
	committed func(sequence int64)
	// sealedThrough is the last sequence this store has sealed into a change
	// block; below it sealChanges skips without I/O.
	sealedThrough atomic.Int64

	follower *follower // nil without WATCH
	closed   atomic.Bool
}

var (
	_ backend.Store        = (*Store)(nil)
	_ backend.ViewProvider = (*Store)(nil)
	_ backend.ChangeSource = (*Store)(nil)
	_ backend.Follower     = (*Store)(nil)
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
	if options.Logger == nil {
		return nil, fmt.Errorf("open bucket store: %w: logger is nil", blob.ErrPrecondition)
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
	if options.MaxDocuments < 0 {
		return nil, fmt.Errorf("open bucket store: %w: max documents must not be negative", blob.ErrPrecondition)
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = defaultRequestTimeout
	}
	if options.ShardWorkers == 0 {
		options.ShardWorkers = defaultShardWorkers
	}
	if options.followInterval == 0 {
		options.followInterval = followInterval
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open bucket store: %w", err)
	}

	store := &Store{
		objects:        objects,
		worldID:        options.WorldID,
		requestTimeout: options.RequestTimeout,
		shardWorkers:   options.ShardWorkers,
		logger:         options.Logger,
		maxDocuments:   options.MaxDocuments,
		readOnly:       options.ReadOnly,
		commitToken:    make(chan struct{}, 1),
		commitInterval: defaultCommitInterval,
		now:            time.Now,
		newOperationID: randomOperationID,
		committed:      options.Committed,
	}
	store.changes, store.backlog = newHub(store, options.ChangeRing)
	store.commitToken <- struct{}{}
	store.refreshMu.Lock()
	loaded, err := loadOrCreate(ctx, objects, &options)
	if err != nil {
		store.refreshMu.Unlock()
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	// The first index reads every body, so it runs under the caller's
	// context rather than the per-request timeout that bounds one refresh.
	if err := store.indexSections(ctx, loaded, sectionSources{}, store.shardWorkers); err != nil {
		store.refreshMu.Unlock()
		return nil, fmt.Errorf("open bucket store: %w", err)
	}
	store.snapshot.Store(loaded)
	store.report(loaded)
	store.refreshMu.Unlock()
	if store.changes != nil {
		store.startFollowing(options.followInterval)
	}
	return store, nil
}

// loadOrCreate loads the world, first creating it in an empty bucket unless
// the store is read-only; an existing world costs no extra head read.
func loadOrCreate(ctx context.Context, objects blob.Store, options *Options) (*snapshot, error) {
	load := func() (*snapshot, error) {
		requestCtx, cancel := context.WithTimeout(ctx, options.RequestTimeout)
		defer cancel()
		return loadRootSnapshot(requestCtx, objects, options.WorldID, options.ShardWorkers)
	}
	loaded, err := load()
	// Only a missing head is a clear not-found; a missing referenced object
	// is an integrity failure and never a reason to create.
	if options.ReadOnly || !errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrIntegrity) {
		return loaded, err
	}
	created, err := ensureWorld(ctx, objects, options.WorldID)
	if err != nil {
		return nil, err
	}
	if created {
		// Loud on purpose: an empty bucket is normally a first install,
		// but it is also what a wrong bucket URL looks like.
		options.Logger.Warn("created a new world in an empty bucket", "worldID", options.WorldID)
	}
	return load()
}

func loadRootSnapshot(ctx context.Context, objects blob.Store, worldID string, workers int) (*snapshot, error) {
	head, headAttributes, err := loadHeadObject(ctx, objects, worldID)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("head is not initialized: %w", err)
		}
		return nil, err
	}

	root, err := getImmutable(ctx, objects, keyedRef{head.Root, rootKey(head.Root.Hash)}, func(root *rootObject) error {
		return validateRootObject(root, head.WorldID)
	})
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := loadShards(ctx, objects, shardLoad{refs: root.Shards, workers: workers})
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
	head, attributes, err := getValidated(ctx, objects, headObjectKey, validateHeadObject)
	if err != nil {
		return head, blob.Attributes{}, err
	}
	if head.WorldID != worldID {
		return head, blob.Attributes{}, fmt.Errorf("%w: configured world ID %q does not match head world ID %q", blob.ErrPrecondition, worldID, head.WorldID)
	}
	return head, attributes, nil
}

// getValidated reads the object at a fixed key; a defect in its encoding
// or content is an integrity failure.
func getValidated[T any](ctx context.Context, objects blob.Store, key string, validate func(*T) error) (T, blob.Attributes, error) {
	var value T
	object, err := objects.Get(ctx, key)
	if err != nil {
		return value, blob.Attributes{}, fmt.Errorf("read %q: %w", key, err)
	}
	if err := validateReadObject(key, &object); err != nil {
		return value, blob.Attributes{}, err
	}
	if err := decodeImmutable(object.Data, &value); err != nil {
		return value, blob.Attributes{}, fmt.Errorf("%w: decode %q: %v", blob.ErrIntegrity, key, err)
	}
	if err := validate(&value); err != nil {
		return value, blob.Attributes{}, fmt.Errorf("%w: validate %q: %v", blob.ErrIntegrity, key, err)
	}
	return value, object.Attributes, nil
}

func (store *Store) refreshSnapshot(ctx context.Context) (*snapshot, error) {
	cached := store.snapshot.Load()
	headAttributes, err := store.objects.Head(ctx, headObjectKey)
	if err != nil {
		return nil, fmt.Errorf("head snapshot: %w", err)
	}
	if err := validateHeadOnly(headAttributes); err != nil {
		return nil, err
	}
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
	headAttributes, err = store.objects.Head(ctx, headObjectKey)
	if err != nil {
		return nil, fmt.Errorf("head snapshot: %w", err)
	}
	if err := validateHeadOnly(headAttributes); err != nil {
		return nil, err
	}
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
	root, err := getImmutable(ctx, store.objects, keyedRef{head.Root, rootKey(head.Root.Hash)}, func(root *rootObject) error {
		return validateRootObject(root, head.WorldID)
	})
	if err != nil {
		return nil, fmt.Errorf("load root: %w", err)
	}
	shards, err := loadShards(ctx, store.objects, shardLoad{refs: root.Shards, previous: cached, workers: store.shardWorkers})
	if err != nil {
		return nil, err
	}
	loaded, err := buildDerivedSnapshot(&head, currentAttributes, &root, shards)
	if err != nil {
		return nil, fmt.Errorf("%w: build snapshot: %v", blob.ErrIntegrity, err)
	}
	if err := store.indexSections(ctx, loaded, sectionSources{previous: cached}, store.shardWorkers); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.snapshot.Store(loaded)
	store.report(loaded)
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

// shardLoad names the shards to read; a ref unchanged from previous is reused.
type shardLoad struct {
	refs     []shardRef
	previous *snapshot
	workers  int
}

func loadShards(ctx context.Context, objects blob.Store, load shardLoad) (*[shardCount]shardObject, error) {
	loaded := new([shardCount]shardObject)
	changed := make([]int, 0, shardCount)
	for index := range shardCount {
		if load.previous != nil && load.refs[index] == load.previous.Root.Shards[index] {
			loaded[index] = load.previous.Shards[index]
			continue
		}
		changed = append(changed, index)
	}
	err := runParallel(ctx, load.workers, changed, func(ctx context.Context, index int) error {
		ref := load.refs[index]
		expectedShard := fmt.Sprintf("%02x", index)
		shard, err := getImmutable(ctx, objects, keyedRef{ref.objectRef, shardKey(expectedShard, ref.Hash)}, func(shard *shardObject) error {
			return validateShardObject(shard, expectedShard)
		})
		if err != nil {
			return fmt.Errorf("load shard %s: %w", expectedShard, err)
		}
		loaded[index] = shard
		return nil
	})
	if err != nil {
		return nil, err
	}
	return loaded, nil
}

// runParallel applies fn to every job on up to load.workers goroutines. The first
// error cancels the rest and is returned; fn's errors are not retried.
func runParallel[T any](ctx context.Context, workers int, jobs []T, fn func(ctx context.Context, job T) error) error {
	if workers < 1 {
		return fmt.Errorf("%w: workers must be positive", blob.ErrPrecondition)
	}
	if len(jobs) == 0 {
		return nil
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	queue := make(chan T, len(jobs))
	for _, job := range jobs {
		queue <- job
	}
	close(queue)
	var wait sync.WaitGroup
	for range min(workers, len(jobs)) {
		wait.Go(func() {
			for job := range queue {
				if workCtx.Err() != nil {
					return
				}
				if err := fn(workCtx, job); err != nil {
					cancel(err)
					return
				}
			}
		})
	}
	wait.Wait()
	return context.Cause(workCtx)
}

// keyedRef is a stored reference and the key its kind and hash must produce.
type keyedRef struct {
	objectRef
	expectedKey string
}

func getImmutable[T any](ctx context.Context, objects blob.Store, keyed keyedRef, validate func(*T) error) (T, error) {
	ref := keyed.objectRef
	var result T
	if err := verifyRef(ref, keyed.expectedKey); err != nil {
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

// newHub is the world's change hub under the world ID and head sequences,
// so cursors agree across replicas and restarts (backend.ChangeSource);
// sealed change blocks reach back a ring. Nil when WATCH is off.
func newHub(store *Store, ring int) (*changefeed.Hub, *changeLog) {
	if ring <= 0 {
		return nil, nil
	}
	backlog := newChangeLog(store, ring)
	return changefeed.NewWithBacklog(store.worldID, ring, backlog), backlog
}

// Changes is the hub this store feeds, or nil when WATCH is off.
func (store *Store) Changes() *changefeed.Hub { return store.changes }
