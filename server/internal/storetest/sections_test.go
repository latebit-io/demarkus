package storetest

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/latebit-io/demarkus/server/internal/catalog"
)

// TestSectionIndexMemory pins the plan's budget: the catalog with its
// section index stays under three times the body bytes it indexed.
func TestSectionIndexMemory(t *testing.T) {
	docs := Corpus(400)
	bodies := 0
	for i := range docs {
		bodies += len(docs[i].Body)
	}
	ratio, _ := measureIndex(t, docs)
	t.Logf("corpus: %d docs, %d body bytes, index %.2fx body bytes", len(docs), bodies, ratio)
	if ratio > 3 {
		t.Errorf("section index uses %.2fx body bytes, budget is 3x", ratio)
	}
}

// TestSectionIndexCorpusDir measures boot cost and query latency over a
// directory of real markdown documents named by DEMARKUS_CORPUS_DIR; it
// skips when unset. Run with -v to read the numbers.
func TestSectionIndexCorpusDir(t *testing.T) {
	dir := os.Getenv("DEMARKUS_CORPUS_DIR")
	if dir == "" {
		t.Skip("DEMARKUS_CORPUS_DIR not set")
	}
	var docs []CorpusDoc
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		docs = append(docs, CorpusDoc{Path: "/" + filepath.ToSlash(rel), Body: string(body)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	bodies := 0
	for i := range docs {
		bodies += len(docs[i].Body)
	}
	ratio, cat := measureIndex(t, docs)
	sections := 0
	for i := range docs {
		sections += cat.Sections(docs[i].Path).Len()
	}
	t.Logf("corpus %s: %d docs, %d sections, %d body bytes, index %.2fx body bytes", dir, len(docs), sections, bodies, ratio)
	for _, q := range []string{"udp buffer", "hairpin", "lookup ranking importance", "path.Match"} {
		start := time.Now()
		const runs = 200
		var rows int
		for range runs {
			rs, err := cat.Lookup(q, catalog.Options{Match: catalog.MatchBody})
			if err != nil {
				t.Fatalf("lookup %q: %v", q, err)
			}
			rows = len(rs)
		}
		t.Logf("query %q: %d rows, %s per lookup", q, rows, time.Since(start)/runs)
	}
}

// measureIndex reports the section index's retained heap relative to body
// bytes: a catalog of the same entries without sections is measured first
// and subtracted, since the budget is on the index, not on catalog mode.
func measureIndex(t *testing.T, docs []CorpusDoc) (float64, *catalog.Catalog) {
	t.Helper()
	bodies := make([][]byte, len(docs))
	total := 0
	for i := range docs {
		bodies[i] = []byte(docs[i].Body)
		total += len(bodies[i])
	}
	entriesOnly := catalog.New()
	catalogBytes := retainedBy(func() {
		for i := range docs {
			entriesOnly.Set(catalog.FromDocument(docs[i].Path, docs[i].Meta, bodies[i], time.Now()))
		}
	})
	start := time.Now()
	cat := catalog.New()
	withIndex := retainedBy(func() {
		for i := range docs {
			cat.Put(docs[i].Path, docs[i].Meta, bodies[i], time.Now())
		}
	})
	elapsed := time.Since(start)
	// Both catalogs and the bodies stay live through the readings; freeing
	// any of them early would flatter the delta.
	runtime.KeepAlive(entriesOnly)
	runtime.KeepAlive(bodies)
	runtime.KeepAlive(docs)
	index := withIndex - catalogBytes
	t.Logf("indexed %d docs in %s: catalog entries %d bytes, section index %d bytes", len(docs), elapsed, catalogBytes, index)
	return float64(index) / float64(max(total, 1)), cat
}

// retainedBy runs build between two post-GC heap readings.
func retainedBy(build func()) int64 {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	build()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}
