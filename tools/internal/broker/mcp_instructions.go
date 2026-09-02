package broker

// Server-instructions text returned in the MCP initialize result: hosts
// with no demarkus plugin (Claude Desktop, ChatGPT, Cursor) get their
// usage contract from the endpoint itself. Kept short; rides every session.

const knowledgeInstructions = `This is an organizational demarkus knowledge system: a shared, versioned markdown knowledge base composed of worlds, addressed as mark://{worldName}/{path}.

Reading: call mark_worlds once to discover the worlds you can read and write. For a subject search, mark_lookup_all is the system-wide card catalog (one ranked table across every readable world); mark_lookup scopes to one world. Then mark_explore the best match to orient, and mark_fetch url#<anchor> for just the sections you need. Anchor on a world's mark://{worldName}/index.md hub to navigate it. Only a successful non-partial empty lookup means nothing was found; surface partial or failed lookups instead of treating them as empty.

Writing: this catalog is shared and authoritative, so publish only knowledge that is ready for others. Fetch mark://root/.well-known/demarkus/policy.md (write policy) and template.md (world layout) with force=true before publishing. On every mark_publish set a metadata object: tags (comma-separated subjects) and importance (0-1; reserve 0.8+ for hubs, architecture, key decisions), plus any tag axes the policy requires. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Never publish secrets or personal data.`

const memoryInstructions = `This is your private demarkus memory: a personal, versioned markdown memory store. Your identity maps to exactly one world; call mark_worlds once to learn its name, then address every document as mark://{worldName}/{path}. Nobody else can read or write it.

Recall first: before answering "what do I know / did we decide / have we seen X", check the memory instead of relying on the current conversation. mark_lookup with url mark://{worldName}/ and a subject query is the card catalog (a ranked table of matching documents); mark_fetch the rows worth reading, and keep mark://{worldName}/index.md as the navigation hub. If nothing relevant comes back, say so; never invent memory.

Record as you go: when something is worth keeping, write it without being asked. Routes: decisions to /adr/<NNNN>-<title>.md, lessons from bugs to /debugging.md, patterns and conventions to /patterns.md, session progress to /journal/<YYYY-MM-DD>.md (append entries with mark_append), plans to /plans/<name>.md, reflections to /thoughts.md. Keep /index.md current as documents are added. The full layout lives at /.well-known/demarkus/template.md.

On every mark_publish set a metadata object: tags (comma-separated subjects drawn from the content) and importance (0-1; reserve 0.8+ for the hub and key decisions). An untagged document is only findable by its exact path. Never put metadata in the document body: no YAML frontmatter fence; a document's name is its # H1 and its kind is the metadata type key. Never set metadata.retention unless the user explicitly asked; it permanently deletes older versions. Do not journal trivia; capture the non-obvious. Do not store secrets or credentials.`
