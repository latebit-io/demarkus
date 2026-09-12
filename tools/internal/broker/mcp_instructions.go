package broker

import "github.com/latebit-io/demarkus/client/mcpfmt"

// Server-instructions text returned in the MCP initialize result: hosts
// with no demarkus plugin (Claude Desktop, ChatGPT, Cursor) get their
// usage contract from the endpoint itself. Kept short; rides every session.

const knowledgeInstructions = `This is an organizational demarkus knowledge system: a shared, versioned markdown knowledge base composed of worlds, addressed as mark://{worldName}/{path}.

Reading: use mark_lookup for an explicit world; otherwise call mark_lookup_all directly. Never widen explicit scope unasked. ` + mcpfmt.SectionFirst + ` Use descriptive subjects, catalog for names/tags, match=body for section text or catalog misses. Fetch the best section; mark_explore only for orientation. A short unanchored document can be read whole. Optional budget=1500 is for focused body queries; the world's index.md is the untagged-content backstop.

Legacy system-wide fallback applies only when mark_lookup_all is unavailable. Require a successful, well-formed mark_worlds directory; failure stops the fallback and an empty directory means no readable scope. Choose one global limit L, request the same query/filter and candidate limit L from each readable world, retain row ordinals and qualify URLs. Merge all successful rows by ordinal ascending, importance descending, world ascending, path ascending; truncate once to L total rows, not L output rows per world. Disclose failed or catalog-only worlds and retain successful rows; an empty partial merge is inconclusive. If all worlds fail, surface the aggregate error and stop.

Read outcomes: ` + mcpfmt.ReadOutcomes + `

Writing: this catalog is shared and authoritative, so publish only knowledge that is ready for others. Fetch mark://root/.well-known/demarkus/policy.md (write policy) and template.md (world layout) with force=true before publishing. On every mark_publish set a metadata object: tags (comma-separated subjects) and importance (0-1; reserve 0.8+ for hubs, architecture, key decisions), plus any tag axes the policy requires. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Never publish secrets or personal data.`

const memoryInstructions = `This is your private demarkus memory: a personal, versioned markdown memory store. Your identity maps to exactly one world; call mark_worlds once to learn its name, then address every document as mark://{worldName}/{path}. Nobody else can read or write it.

Recall first: check this memory before answering recall questions. ` + mcpfmt.SectionFirst + ` Use your world's URL or an established subtree and descriptive subjects; catalog for names/tags, match=body for section text or catalog misses. Fetch the best section, choose from an outline, or read a short unanchored document whole. Optional budget=1500 is for focused body queries; your world's index.md is the untagged-content backstop. Never invent memory.

Read outcomes: ` + mcpfmt.ReadOutcomes + `

Record as you go: when something is worth keeping, write it without being asked. Routes: decisions to /adr/<NNNN>-<title>.md, lessons from bugs to /debugging.md, patterns and conventions to /patterns.md, session progress to /journal/<YYYY-MM-DD>.md (append entries with mark_append), plans to /plans/<name>.md, reflections to /thoughts.md. Keep /index.md current as documents are added. The full layout lives at /.well-known/demarkus/template.md.

On every mark_publish set a metadata object: tags (comma-separated subjects drawn from the content) and importance (0-1; reserve 0.8+ for the hub and key decisions). An untagged document is only findable by its exact path. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Do not journal trivia; capture the non-obvious. Do not store secrets or credentials.`
