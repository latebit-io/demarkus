package storetest

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/latebit-io/demarkus/protocol"
	"github.com/latebit-io/demarkus/protocol/store"
	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// DifferentialConfig tunes a differential run. Zero values take defaults.
type DifferentialConfig struct {
	Seeds []int64 // one subtest per seed; each suite has its own default list
	Ops   int     // operations per seed; default 200
}

// snapshotEvery is the full-state comparison cadence in ops.
const snapshotEvery = 10

// RunDifferential drives one seeded random op sequence through a reference and
// a candidate backend, comparing every return and periodically the full state.
// Conformance asserts what its author imagined; this catches the rest.
func RunDifferential(t *testing.T, ref, cand LookupFactory, cfg DifferentialConfig) {
	runSeeds(t, cfg, []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233}, func(t *testing.T, seed int64) {
		RunDifferentialSeed(t, ref(t), cand(t), seed, cfg)
	})
}

// RunDifferentialSeed runs one seeded sequence against already-constructed
// backends. Exposed so a fuzz target can feed arbitrary seeds.
func RunDifferentialSeed(t *testing.T, ref, cand LookupBackend, seed int64, cfg DifferentialConfig) {
	t.Helper()
	d := &differential{diffRun: newDiffRun(t, seed), ref: ref, cand: cand}
	d.run(cfg.Ops, d.apply, d.compareSnapshots)
}

// runSeeds runs fn once per configured seed as a subtest.
func runSeeds(t *testing.T, cfg DifferentialConfig, defaults []int64, fn func(t *testing.T, seed int64)) {
	seeds := cfg.Seeds
	if len(seeds) == 0 {
		seeds = defaults
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) { fn(t, seed) })
	}
}

type differential struct {
	*diffRun
	ref  LookupBackend
	cand LookupBackend
}

// diffRun is the seeded op generator, op loop, and failure reporter shared by
// the store-level and handler-level differentials.
type diffRun struct {
	t       *testing.T
	rng     *rand.Rand
	seed    int64
	history []string
}

func newDiffRun(t *testing.T, seed int64) *diffRun {
	return &diffRun{t: t, rng: rand.New(rand.NewSource(seed)), seed: seed}
}

// run draws ops ops, applying each and snapshotting every snapshotEvery,
// stopping at the first failure.
func (d *diffRun) run(ops int, apply func(op), snapshot func()) {
	if ops <= 0 {
		ops = 200
	}
	for i := range ops {
		apply(d.nextOp())
		if !d.t.Failed() && ((i+1)%snapshotEvery == 0 || i == ops-1) {
			snapshot()
		}
		if d.t.Failed() {
			return
		}
	}
}

// currentBoth returns the shared current version of path, failing when the
// backends already disagree before an op is applied.
func (d *diffRun) currentBoth(ref, cand LookupBackend, path string) (int, bool) {
	refCur, refErr := ref.Store.CurrentVersion(path)
	candCur, candErr := cand.Store.CurrentVersion(path)
	if refErr != nil || candErr != nil {
		d.fail("CurrentVersion(%s) errors: ref=%v cand=%v", path, refErr, candErr)
		return 0, false
	}
	if refCur != candCur {
		d.fail("CurrentVersion(%s) before op: ref=%d cand=%d", path, refCur, candCur)
		return 0, false
	}
	return refCur, true
}

// compareLines reports the first differing snapshot line.
func (d *diffRun) compareLines(ref, cand []string) {
	for i := range ref {
		if i >= len(cand) {
			d.fail("snapshot: candidate shorter at line %d\nref: %s", i, ref[i])
			return
		}
		if ref[i] != cand[i] {
			d.fail("snapshot line %d differs\nref:  %s\ncand: %s", i, ref[i], cand[i])
			return
		}
	}
	if len(cand) > len(ref) {
		d.fail("snapshot: candidate longer at line %d\ncand: %s", len(ref), cand[len(ref)])
	}
}

// opKind enumerates the mutating operations the generator emits. Reads are
// covered by the snapshot, which exercises every read method over the pool.
type opKind int

const (
	opWrite opKind = iota
	opAppend
	opArchive
	opUnarchive
)

type op struct {
	kind    opKind
	path    string
	expMode string
	body    int // index into bodies
	meta    int // index into metas
	okf     bool
}

func (k opKind) String() string {
	return [...]string{"write", "append", "archive", "unarchive"}[k]
}

func (o op) String() string {
	if o.kind == opArchive || o.kind == opUnarchive {
		return o.kind.String() + " " + o.path
	}
	return fmt.Sprintf("%s %s exp=%s body=%d meta=%d okf=%v", o.kind, o.path, o.expMode, o.body, o.meta, o.okf)
}

// Pools. Paths deliberately include collisions both ways, dot names, unicode,
// OKF-exempt basenames, a non-.md name, traversal, and unnormalized spellings.
var (
	diffDocPaths = []string{
		"/a.md", "/b.md", "/d/x.md", "/d/y.md", "/d/e/z.md",
		"/.h.md", "/d/.s.md", "/ü/ñ.md", "/index.md", "/d/log.md",
		"/a.md/child.md", "/d", "/x", "/d/../b.md", "//a.md", "/d/e/", "/missing.md",
	}
	diffDirPaths = []string{
		"/", "/d", "/d/e", "/ü", "/a.md", "/nope", "/.h.md", "/d/e/", "/d/../d", "",
	}
	diffBodies = [][]byte{
		[]byte("# A\n"),
		[]byte("# A\nline\n"),
		[]byte("---\ntitle: own\n---\n# Doc\n"),
		[]byte("x"),
		[]byte(""),
		[]byte("line1\nline2"),
		[]byte("line1\nline2\n"),
		[]byte(strings.Repeat("# Title with Words\n\nbody paragraph\n", 64)),
		[]byte("# Üñí cödé\n"),
		[]byte("no heading, just text"),
		{0x89, 'P', 'N', 'G', 0xff, 0xfe, 0x00},
	}
	diffMetas = []map[string]string{
		nil,
		{},
		{"tags": "alpha, beta", "importance": "0.9", "title": "Kept"},
		{"tags": "Alpha"},
		{"tags": "gamma, alpha", "importance": "0.5", "type": "Plan"},
		{"tags": "beta", "importance": "0.5", "title": "Words Title"},
		{"importance": "0.7"},
		{"retention": "2"},
		{"retention": "1", "tags": "x"},
		{"retention": "abc"},
		{"version": "3"},
		{"agent": "claude"},
		{"bad key": "v"},
		{"tags": "nl\ninjected"},
		{"tags": strings.Repeat("x", protocol.MaxMetaBytes+8)},
		{"rel-depends-on": "/a.md, /b.md", "importance": "1"},
	}
	diffQueries = []string{"*", "alpha", "beta", "Alpha gamma", "words", "title", "kept words", "nothing", ""}
	diffScopes  = []string{"", "/", "/d", "/d/", "/ü", "/a.md", "/nope"}
	diffFilters = []string{"", "tag=alpha", "type=Plan", "modified-after=2000-01-01", "modified-before=2000-01-01", "tag=alpha,importance=0.9"}
)

// oversizedBody is shared so the 1 MiB+1 allocation happens once; it is
// emitted rarely since writing it to Postgres is comparatively slow.
var oversizedBody = []byte(strings.Repeat("y", protocol.MaxBodyLength+1))

// nextOp draws the next op and records it in the history for failure output.
func (d *diffRun) nextOp() op {
	r := d.rng
	o := op{path: diffDocPaths[r.Intn(len(diffDocPaths))]}
	switch n := r.Intn(100); {
	case n < 50:
		o.kind = opWrite
	case n < 75:
		o.kind = opAppend
	case n < 88:
		o.kind = opArchive
	default:
		o.kind = opUnarchive
	}
	if o.kind == opWrite || o.kind == opAppend {
		o.body = r.Intn(len(diffBodies))
		if r.Intn(100) == 0 {
			o.body = len(diffBodies) // the oversized body, kept rare: 1 MiB per write
		}
		o.meta = r.Intn(len(diffMetas))
		o.okf = r.Intn(2) == 0
		switch n := r.Intn(100); {
		case n < 35:
			o.expMode = "cur"
		case n < 60:
			o.expMode = "any"
		case n < 70:
			o.expMode = "create"
		case n < 80:
			o.expMode = "stale"
		case n < 90:
			o.expMode = "ahead"
		default:
			o.expMode = "far"
		}
	}
	d.history = append(d.history, o.String())
	return o
}

// expectedFor resolves the symbolic expected-version mode against a backend's
// own current version so both backends are asked the same question.
func expectedFor(mode string, cur int) int {
	switch mode {
	case "any":
		return -1
	case "create":
		return 0
	case "stale":
		if cur == 0 {
			return 1 // cur-1 would be -1, the skip-check sentinel
		}
		return cur - 1
	case "ahead":
		return cur + 1
	case "far":
		return cur + 50
	default:
		return cur
	}
}

func bodyFor(o op) []byte {
	if o.body == len(diffBodies) {
		return oversizedBody
	}
	return diffBodies[o.body]
}

func metaFor(o op) map[string]string {
	m := diffMetas[o.meta]
	if o.okf {
		return store.ApplyOKFTypeDefault(o.path, m)
	}
	return m
}

// apply runs one op against both backends, mirroring the handler's catalog
// maintenance, and compares the results.
func (d *differential) apply(o op) {
	cur, ok := d.currentBoth(d.ref, d.cand, o.path)
	if !ok {
		return
	}
	refRes := d.applyOne(d.ref, o, cur)
	candRes := d.applyOne(d.cand, o, cur)
	if refRes != candRes {
		d.fail("op result differs\nref:  %s\ncand: %s", refRes, candRes)
	}
}

func (d *differential) applyOne(b LookupBackend, o op, cur int) string {
	switch o.kind {
	case opWrite:
		doc, err := publishInto(b, o.path, expectedFor(o.expMode, cur), bodyFor(o), metaFor(o))
		return errClass(err) + " " + describeDoc(doc)
	case opAppend:
		doc, err := appendInto(b, o.path, expectedFor(o.expMode, cur), bodyFor(o), metaFor(o))
		return errClass(err) + " " + describeDoc(doc)
	case opArchive:
		return errClass(archiveInto(b, o.path))
	default:
		return errClass(unarchiveInto(b, o.path))
	}
}

// errClass maps an error to the sentinel the handler would act on. Backends
// may wrap differently; what must agree is the class.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	case errors.Is(err, store.ErrNotModified):
		return "not-modified"
	case errors.Is(err, store.ErrArchived):
		return "archived"
	case errors.Is(err, store.ErrInvalidMeta):
		return "invalid-meta"
	case errors.Is(err, store.ErrInvalidContent):
		return "invalid-content"
	case errors.Is(err, store.ErrSizeLimit):
		return "size-limit"
	case errors.Is(err, os.ErrNotExist):
		return "not-exist"
	default:
		return "error"
	}
}

// describeDoc renders the backend-neutral parts of a Document. Modified is
// wall-clock and differs between backends by construction, so only its
// presence is compared.
func describeDoc(doc *store.Document) string {
	if doc == nil {
		return "<nil>"
	}
	prune := "prune=nil"
	if doc.Prune != nil {
		prune = fmt.Sprintf("prune=%d-%d err=%v", doc.Prune.From, doc.Prune.To, doc.Prune.Err != nil)
	}
	return fmt.Sprintf("v=%d archived=%v modified=%v body=%q meta=%s %s",
		doc.Version, doc.Archived, !doc.Modified.IsZero(), doc.Content, describeMeta(doc.Metadata), prune)
}

// describeMeta renders metadata with sorted keys (fmt sorts map keys); nil
// and empty both print as map[], which is the intended equivalence.
func describeMeta(m map[string]string) string {
	return fmt.Sprintf("%q", m)
}

// compareSnapshots renders the full observable state of both backends over
// the path, body, and query pools and reports the first differing line.
func (d *differential) compareSnapshots() {
	d.compareLines(snapshot(d.ref), snapshot(d.cand))
}

func snapshot(b LookupBackend) []string {
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	for _, p := range diffDocPaths {
		cur, curErr := b.Store.CurrentVersion(p)
		add("current %s = %d %s", p, cur, errClass(curErr))
		doc, err := b.Store.Get(p, 0)
		add("get %s = %s %s", p, errClass(err), describeDoc(doc))
		vs, err := b.Store.Versions(p)
		nums := make([]int, len(vs))
		for i, v := range vs {
			nums[i] = v.Version
		}
		add("versions %s = %s %v", p, errClass(err), nums)
		for _, v := range nums {
			vd, err := b.Store.Get(p, v)
			add("get %s@%d = %s %s", p, v, errClass(err), describeDoc(vd))
		}
		_, err = b.Store.Get(p, cur+1)
		add("get %s@next = %s", p, errClass(err))
		add("chain %s = %s", p, errClass(b.Store.VerifyChain(p)))
	}
	for _, dir := range diffDirPaths {
		ok, err := b.Store.IsDir(dir)
		add("isdir %q = %v %s", dir, ok, errClass(err))
		for _, includeArchived := range []bool{false, true} {
			entries, err := b.Store.ListEntries(dir, includeArchived)
			add("list %q archived=%v = %s %v", dir, includeArchived, errClass(err), entries)
		}
	}
	for i, body := range diffBodies {
		p, err := b.Store.LookupHash(store.ContentHash(body))
		add("hash body=%d = %q %s", i, p, errClass(err))
	}
	// Queries x scopes unfiltered, then filters on the match-all query: the
	// full cross product is mostly redundant and dominates snapshot cost.
	lookup := func(q, scope, filter string) {
		preds, perr := catalog.ParseFilter(filter)
		if perr != nil {
			add("filter %q parse error", filter)
			return
		}
		rs, err := b.Catalog.Lookup(q, catalog.Options{Scope: scope, Filter: preds})
		add("lookup q=%q scope=%q filter=%q = %s %s", q, scope, filter, errClass(err), describeResults(rs))
	}
	for _, q := range diffQueries {
		for _, scope := range diffScopes {
			lookup(q, scope, "")
		}
	}
	for _, f := range diffFilters {
		lookup("*", "", f)
		lookup("alpha", "/d", f)
	}
	return lines
}

// describeResults renders lookup results in an order independent of the
// modified-time tiebreak, which wall-clock skew between backends makes
// non-deterministic. Score, importance, and path order are still asserted.
func describeResults(rs []catalog.Result) string {
	sorted := make([]catalog.Result, len(rs))
	copy(sorted, rs)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Importance != b.Importance {
			return a.Importance > b.Importance
		}
		return a.Path < b.Path
	})
	parts := make([]string, len(sorted))
	for i, r := range sorted {
		parts[i] = fmt.Sprintf("{%s score=%d imp=%g title=%q tags=%v meta=%s mod=%v}",
			r.Path, r.Score, r.Importance, r.Title, r.Tags, describeMeta(r.Metadata), !r.Modified.IsZero())
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func (d *diffRun) fail(format string, args ...any) {
	d.t.Helper()
	n := len(d.history)
	from := max(0, n-12)
	d.t.Errorf("seed %d, op %d: %s\nlast ops:\n  %s", d.seed, n, fmt.Sprintf(format, args...),
		strings.Join(d.history[from:], "\n  "))
}
