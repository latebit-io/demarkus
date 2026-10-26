package mcpfmt

// SectionFirst keeps ordinary recall guidance consistent without changing API
// defaults; budget only when the answer spans documents.
const SectionFirst = "Prefer scoped lookup (limit 3): catalog for names/tags, match=body for section text or catalog misses. Answer in one section: omit budget, fetch the best returned #anchor. Answer spanning documents or their relations (supersedes, depends on, implements, derives from, influenced by, roles): budget=1500 on the first lookup; cite its '>>>' blocks, fetch a #anchor only for a passage they omit. Path-only results: use match=body or mark_explore to locate a section, or fetch without force for a short body or outline; select a returned anchor if outlined. Never invent anchors; expand only focused evidence."

// ReadOutcomes distinguishes failed primary reads from usable degraded responses.
const ReadOutcomes = "Primary lookup or document-fetch failure: disclose and stop that operation; never infer absence. Recoverable expansion fetch/section or exploration sibling-listing failures: retain valid tables/cards, disclose gaps and continue from usable results. Partial empty results and body requests answered wholly or partly by catalog are inconclusive; missing body-mode confirmation is catalog fallback. Only a successful, non-partial empty lookup without mode fallback establishes no matches."
