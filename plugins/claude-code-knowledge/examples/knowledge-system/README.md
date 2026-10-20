# Knowledge System Conventions

Reference artifacts for an organizational demarkus knowledge system (a broker-fronted universe joined with `/knowledge-join`). These are **not** auto-installed; the plugin can't write to your broker. An admin publishes them to the system's `root` hub; joined agents read and follow them.

## The `root` hub

Every knowledge system has a guaranteed hub world named **`root`** that tracks things globally for the system. Org-wide conventions live there as ordinary versioned documents under a well-known prefix:

```text
mark://root/.well-known/demarkus/policy.md     ← strictness, required tags, allowed values (see policy.md here)
mark://root/.well-known/demarkus/template.md   ← required per-world structure (the canonical layout)
mark://root/.well-known/demarkus/style.md      ← the style guide; seeded by the server, its rule table generated from the style gate
```

The `root` policy is the **floor**: the server merges it under every other world's own `/.well-known/demarkus/policy.md` on every write (the stricter strictness, the union of required axes and fields, the intersection of allowed values), so a world can tighten the org's rules but never loosen them. The server also refuses any body carrying a secret or personal data, whatever the policy says.

`template.md` is the canonical per-world layout (world hub, descriptor, `/projects/<slug>/` subtrees, hub and metadata rules); publish a copy of it to `root`, tailored as needed. `policy.md` and `template.md` are the examples in this directory.

## Per-world descriptors (`world.md`)

`policy.md` and `template.md` live once on `root` and govern the whole system. A **world descriptor** is different: it is per-world, published by each world's owner to its own well-known prefix:

```text
mark://<world>/.well-known/demarkus/world.md   ← team, domain, partition role, autonomy ceiling (see world.md here)
```

It declares who owns the world, what subject domain it holds, its partition role (`hub`/`team`/`project`/`scratch`), and the most autonomous a promotion *into it* may be (`autonomy_ceiling`). The curation pipeline reads these to route a promotion to the right world among the ones the caller may write (the `writable` column of `mark_worlds`) and to cap promotion autonomy at the destination's ceiling. A world without a descriptor still works; it appears in the manual pick-list and defaults to `human-only`. `world.md` is the example in this directory.

## How a joined agent consumes them

The server enforces the policy on every write: `strictness:` sets the severity, `require_tags:` names tag axes each publish must carry as `axis:value` (e.g. `category:project`), `allow_tags:` lists the values per axis, `require_fields:` the metadata keys. A write that falls short is refused or warned with the rule to fix. `/knowledge-join` registers the system so the plugin's own publish gate adds the same `tags` presence check before the call leaves the session.

On first substantive work against a knowledge system, the agent fetches `mark://root/.well-known/demarkus/policy.md` and `mark://root/.well-known/demarkus/template.md` and follows them; structure conventions are advisory by necessity (only the agent, via its MCP OAuth session, can reach the broker; a hook cannot).

## Why this shape

A shared catalog with many writers needs consistency conventions more than a personal memory does, and the org should set the bar centrally rather than each member re-deriving it. Hosting the policy and template on `root` means a single edit propagates to everyone, with no plugin release and no per-user setup; the conventions travel through the same knowledge system they govern.
