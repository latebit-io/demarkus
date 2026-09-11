# Write Policy

This world was created with the default policy, so violations warn instead of blocking and no tag axes are required yet.

```yaml
strictness: warn
```

Replace this document once the world has conventions worth enforcing. Publish a new version to the same path and it governs the next write. A malformed replacement is rejected rather than stored, and tightening is always safe because this version gates the write that replaces it.

## Metadata every publish carries

Set a `metadata` object with `tags`, a comma-separated list of subjects drawn from the content, and `importance`, a float from 0 to 1 with 0.8 and above reserved for hubs, architecture, and accepted decisions. An untagged document is findable only by its title or its exact path.

Metadata travels out of band, so none of it belongs in the body. A document that opens with a frontmatter fence stores that fence literally.

## Enforcement you can add

Use `require_tags: category` to demand a category axis on every publish, and `require_fields: type` to demand a document kind. Keep the two satisfiable together, because one publish has to be able to meet every axis and field at once.

Raise strictness to `block` once the taxonomy is settled, or to `ask` when a person should approve a violating write.

## Style baseline

Every document opens with a `# H1` name and a one-sentence summary directly beneath it. Headings are anchors, so they stay unique within a document. No em dashes.

Never set `metadata.retention` unless the owner asks for it. It permanently deletes all but the newest versions, and history is the point of a versioned store.
