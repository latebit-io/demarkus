package catalog

import (
	"iter"

	"github.com/latebit-io/demarkus/protocol/neardup"
)

// similar is a Lookup in similar mode: the documents under the scope and
// filter, Skip aside, that opts.Draft repeats; one Result per hit in
// neardup.Scan's order, the Hit and its anchor set.
func similar(index Index, opts Options) []Result {
	if opts.Draft == nil {
		return nil
	}
	scope := NormalizeScope(opts.Scope)
	hits := neardup.Scan(*opts.Draft, targetsOf(index, scope, &opts))
	if len(hits) == 0 {
		return nil
	}
	byPath := make(map[string]int, len(hits))
	for i, h := range hits {
		byPath[h.Path] = i
	}
	results := make([]Result, len(hits))
	for entry, doc := range index.Entries(scope) {
		i, ok := byPath[entry.Path]
		if !ok {
			continue
		}
		h := hits[i]
		results[i] = Result{Entry: *entry, Anchor: h.Anchor, Heading: doc.heading(h.Anchor), Hit: h}
	}
	return results
}

// targetsOf yields each admitted document that has a section index.
func targetsOf(index Index, scope string, opts *Options) iter.Seq[neardup.Target] {
	return func(yield func(neardup.Target) bool) {
		for entry, doc := range index.Entries(scope) {
			if doc == nil || !opts.admits(entry, scope) {
				continue
			}
			if !yield(neardup.Target{Path: entry.Path, Sig: doc.sig, Sections: doc}) {
				return
			}
		}
	}
}
