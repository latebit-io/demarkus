// Package merge implements three-way line-based merge for Mark Protocol clients.
// The diff3 algorithm here is used by the MCP `mark_publish` handler to
// reconcile concurrent writes without an LLM round trip when changes are
// disjoint, and to surface git-style conflict markers when they overlap.
package merge

import "strings"

// Result holds the output of a three-way merge.
type Result struct {
	Body     string
	Conflict bool
}

// Diff3 performs a line-based three-way merge of base, ours, and theirs.
// Lines that only one side changed are taken from that side. Lines that both
// sides changed identically collapse into a single copy. Lines that both
// sides changed differently are emitted with git-style conflict markers and
// Conflict is set to true.
func Diff3(base, ours, theirs string) Result {
	baseLines := splitLines(base)
	oursLines := splitLines(ours)
	theirsLines := splitLines(theirs)

	hunksO := editHunks(baseLines, oursLines)
	hunksT := editHunks(baseLines, theirsLines)

	var out strings.Builder
	conflict := false

	bPos, oPos, tPos := 0, 0, 0
	oi, ti := 0, 0

	for bPos < len(baseLines) || oi < len(hunksO) || ti < len(hunksT) {
		oAtPos := oi < len(hunksO) && hunksO[oi].baseLo == bPos
		tAtPos := ti < len(hunksT) && hunksT[ti].baseLo == bPos

		if !oAtPos && !tAtPos {
			// Stable region: emit base lines until the next hunk starts
			// or we reach the end.
			stableEnd := len(baseLines)
			if oi < len(hunksO) {
				stableEnd = min(stableEnd, hunksO[oi].baseLo)
			}
			if ti < len(hunksT) {
				stableEnd = min(stableEnd, hunksT[ti].baseLo)
			}
			n := stableEnd - bPos
			for k := range n {
				out.WriteString(baseLines[bPos+k])
			}
			bPos += n
			oPos += n
			tPos += n
			continue
		}

		// Unstable chunk. Consume any hunks anchored at bPos, then extend
		// to absorb hunks that start strictly inside the chunk.
		bLo, oLo, tLo := bPos, oPos, tPos
		bHi := bPos
		lastOBaseHi, lastOSideHi := bLo, oLo
		lastTBaseHi, lastTSideHi := bLo, tLo

		if oAtPos {
			h := hunksO[oi]
			if h.baseHi > bHi {
				bHi = h.baseHi
			}
			lastOBaseHi, lastOSideHi = h.baseHi, h.sideHi
			oi++
		}
		if tAtPos {
			h := hunksT[ti]
			if h.baseHi > bHi {
				bHi = h.baseHi
			}
			lastTBaseHi, lastTSideHi = h.baseHi, h.sideHi
			ti++
		}

		for {
			extended := false
			if oi < len(hunksO) && hunksO[oi].baseLo < bHi {
				h := hunksO[oi]
				if h.baseHi > bHi {
					bHi = h.baseHi
				}
				lastOBaseHi, lastOSideHi = h.baseHi, h.sideHi
				oi++
				extended = true
			}
			if ti < len(hunksT) && hunksT[ti].baseLo < bHi {
				h := hunksT[ti]
				if h.baseHi > bHi {
					bHi = h.baseHi
				}
				lastTBaseHi, lastTSideHi = h.baseHi, h.sideHi
				ti++
				extended = true
			}
			if !extended {
				break
			}
		}

		// Trailing-stable extension: between the last consumed hunk and
		// bHi, ours and theirs match base 1-to-1, so the side ranges
		// extend by the remaining base count.
		oHi := lastOSideHi + (bHi - lastOBaseHi)
		tHi := lastTSideHi + (bHi - lastTBaseHi)

		emitChunk(
			baseLines[bLo:bHi],
			oursLines[oLo:oHi],
			theirsLines[tLo:tHi],
			&out, &conflict,
		)
		bPos, oPos, tPos = bHi, oHi, tHi
	}

	return Result{Body: out.String(), Conflict: conflict}
}

// splitLines splits s on newlines while preserving the trailing newline on
// each line. The explicit nil for empty input keeps callers from special-
// casing length.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")
}

// editHunk describes a region where base differs from one side. base[baseLo:baseHi]
// is replaced by side[sideLo:sideHi]. Either range may be empty (pure
// insertion or deletion).
type editHunk struct {
	baseLo, baseHi int
	sideLo, sideHi int
}

// editHunks computes the diff between base and side as an ordered list of
// replacement hunks, derived from the LCS of the two line sequences.
func editHunks(base, side []string) []editHunk {
	matches := lcsMatches(base, side)
	var hunks []editHunk
	bPrev, sPrev := 0, 0
	for _, m := range matches {
		if m.baseIdx > bPrev || m.sideIdx > sPrev {
			hunks = append(hunks, editHunk{
				baseLo: bPrev, baseHi: m.baseIdx,
				sideLo: sPrev, sideHi: m.sideIdx,
			})
		}
		bPrev = m.baseIdx + 1
		sPrev = m.sideIdx + 1
	}
	if bPrev < len(base) || sPrev < len(side) {
		hunks = append(hunks, editHunk{
			baseLo: bPrev, baseHi: len(base),
			sideLo: sPrev, sideHi: len(side),
		})
	}
	return hunks
}

type lineMatch struct{ baseIdx, sideIdx int }

// lcsMatches returns the longest common subsequence of base and side as
// (baseIdx, sideIdx) pairs in ascending order. O(n*m) time and space; for
// markdown documents under the 1 MiB body limit this is microseconds.
func lcsMatches(base, side []string) []lineMatch {
	m, n := len(base), len(side)
	if m == 0 || n == 0 {
		return nil
	}
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			switch {
			case base[i-1] == side[j-1]:
				dp[i][j] = dp[i-1][j-1] + 1
			case dp[i-1][j] >= dp[i][j-1]:
				dp[i][j] = dp[i-1][j]
			default:
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	matches := make([]lineMatch, 0, dp[m][n])
	i, j := m, n
	for i > 0 && j > 0 {
		switch {
		case base[i-1] == side[j-1]:
			matches = append(matches, lineMatch{baseIdx: i - 1, sideIdx: j - 1})
			i--
			j--
		case dp[i-1][j] >= dp[i][j-1]:
			i--
		default:
			j--
		}
	}
	for l, r := 0, len(matches)-1; l < r; l, r = l+1, r-1 {
		matches[l], matches[r] = matches[r], matches[l]
	}
	return matches
}

// emitChunk merges a single unstable region of base/ours/theirs and writes
// the result to out. If both sides made conflicting edits, conflict markers
// are written and *conflict is set.
func emitChunk(baseChunk, oursChunk, theirsChunk []string, out *strings.Builder, conflict *bool) {
	oursEqualsBase := equalLines(oursChunk, baseChunk)
	theirsEqualsBase := equalLines(theirsChunk, baseChunk)

	switch {
	case oursEqualsBase && theirsEqualsBase:
		writeLines(out, baseChunk)
	case oursEqualsBase:
		writeLines(out, theirsChunk)
	case theirsEqualsBase:
		writeLines(out, oursChunk)
	case equalLines(oursChunk, theirsChunk):
		writeLines(out, oursChunk)
	default:
		*conflict = true
		out.WriteString("<<<<<<< ours\n")
		writeLines(out, oursChunk)
		ensureNewline(out)
		out.WriteString("=======\n")
		writeLines(out, theirsChunk)
		ensureNewline(out)
		out.WriteString(">>>>>>> theirs\n")
	}
}

func writeLines(out *strings.Builder, lines []string) {
	for _, l := range lines {
		out.WriteString(l)
	}
}

// ensureNewline appends a newline if the buffer does not already end with one.
// Conflict markers must start at the beginning of a line, so a chunk whose
// last line is missing its trailing newline would otherwise run into the
// next marker.
func ensureNewline(out *strings.Builder) {
	s := out.String()
	if s == "" || s[len(s)-1] == '\n' {
		return
	}
	out.WriteByte('\n')
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
