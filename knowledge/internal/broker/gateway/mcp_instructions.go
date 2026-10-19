package gateway

import "github.com/latebit-io/demarkus/client/mcpfmt"

// Server-instructions text returned in the MCP initialize result: hosts
// with no demarkus plugin (Claude Desktop, ChatGPT, Cursor) get their
// usage contract from the endpoint itself. Kept short; rides every session.

const knowledgeInstructions = `This is an organizational demarkus knowledge system: a shared, versioned markdown knowledge base composed of worlds, addressed as mark://{worldName}/{path}.

Reading: use mark_lookup for an explicit world; otherwise call mark_lookup_all directly. Never widen explicit scope unasked. ` + mcpfmt.SectionFirst + ` Use descriptive subjects; mark_explore only for orientation. The world's index.md is the untagged-content backstop.

Read outcomes: ` + mcpfmt.ReadOutcomes + `

Writing: this catalog is shared and authoritative, so publish only knowledge that is ready for others. Fetch the floor world's /.well-known/demarkus/policy.md (root by default; the target world's own policy.md only tightens it) and template.md (world layout) with force=true before publishing. On every mark_publish set a metadata object: tags (comma-separated subjects) and importance (0-1; reserve 0.8+ for hubs, architecture, key decisions), plus any tag axes the policy requires, with values from its allow_tags lists; agent and user are stamped by the gateway, never set them. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Never publish secrets or personal data. The server refuses a body carrying a secret, naming the rule and line; personal data is refused or warned per the system's policy, and metadata content-check-override with the reason records a reviewed false positive.`

const memoryInstructions = `This is your private demarkus memory: a personal, versioned markdown memory store. Your identity maps to exactly one world; call mark_worlds once to learn its name, then address every document as mark://{worldName}/{path}. Nobody else can read or write it.

Recall first: check this memory before answering recall questions. ` + mcpfmt.SectionFirst + ` Use your world's URL or an established subtree and descriptive subjects. Your world's index.md is the untagged-content backstop. Never invent memory.

Read outcomes: ` + mcpfmt.ReadOutcomes + `

Record as you go: when something is worth keeping, write it without being asked. Routes: decisions to /adr/<NNNN>-<title>.md, lessons from bugs to /debugging.md, patterns and conventions to /patterns.md, session progress to /journal/<YYYY-MM-DD>.md (mark_append with route today on the world root), plans to /plans/<name>.md, reflections to /thoughts.md. Keep /index.md current as documents are added. The full layout lives at /.well-known/demarkus/template.md.

On every mark_publish set a metadata object: tags (comma-separated subjects drawn from the content) and importance (0-1; reserve 0.8+ for the hub and key decisions). An untagged document is only findable by its exact path. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Do not journal trivia; capture the non-obvious. Do not store secrets or credentials: the server refuses a body carrying one, naming the rule and line; remove it and republish.`
