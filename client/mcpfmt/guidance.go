package mcpfmt

// SectionFirst keeps ordinary recall guidance consistent without changing API defaults.
const SectionFirst = "Prefer scoped lookup (limit 3, no budget), then fetch the best #anchor; expand only focused evidence."

// ReadOutcomes distinguishes failed primary reads from usable degraded responses.
const ReadOutcomes = "Primary lookup or document-fetch failure: disclose and stop that operation; never infer absence. Expansion fetch/section errors or exploration sibling-listing failures with valid tables/cards are recoverable: retain results, disclose gaps and continue the read from usable rows/sections. Partial empty results and empty body requests answered wholly or partly by catalog are inconclusive; missing body-mode confirmation is catalog fallback. Only a successful, non-partial empty lookup without mode fallback establishes no matches."
