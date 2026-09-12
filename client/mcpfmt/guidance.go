package mcpfmt

// SectionFirst keeps ordinary recall guidance consistent without changing API defaults.
const SectionFirst = "Prefer scoped lookup (limit 3, no budget). Fetch the best returned #anchor. Path-only results: use match=body or mark_explore to locate a section, or fetch without force for a short body or outline; select a returned anchor if outlined. Never invent anchors; expand only focused evidence."

// ReadOutcomes distinguishes failed primary reads from usable degraded responses.
const ReadOutcomes = "Primary lookup or document-fetch failure: disclose and stop that operation; never infer absence. Recoverable expansion fetch/section or exploration sibling-listing failures: retain valid tables/cards, disclose gaps and continue from usable results. Partial empty results and body requests answered wholly or partly by catalog are inconclusive; missing body-mode confirmation is catalog fallback. For mark_lookup_all, require its reported worlds count to be positive before inferring no matches. Zero means no readable worlds: disclose unavailable scope, not no matches; a missing count is inconclusive. Otherwise, only a successful, non-partial empty lookup without mode fallback establishes no matches."
