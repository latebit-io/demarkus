# Write Policy

This is a personal soul, so ceremony stays low: no tag axes are required and style problems warn rather than block.

```yaml
strictness: warn
```

Tag every publish anyway. Set a `metadata` object with `tags` (comma-separated subjects drawn from the content) and `importance` (a float 0 to 1; reserve 0.8 and above for the hub and key decisions). An untagged document is only findable by its exact path.

Style baseline: every document opens with a `# H1` name, a one-sentence summary sits directly under it, headings are unique because they are anchors, metadata never goes in the body (no YAML frontmatter fence), and no em dashes.

Never set `metadata.retention` unless the owner explicitly asks: it permanently deletes all but the newest N versions, and history is the product here.
