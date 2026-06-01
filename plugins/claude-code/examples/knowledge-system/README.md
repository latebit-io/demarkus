# Knowledge System Conventions

Reference artifacts for an organizational demarkus knowledge system (a broker-fronted universe joined with `/knowledge-join`). These are **not** auto-installed — the plugin can't write to your broker. An admin publishes them to the system's `root` hub; joined agents read and follow them.

## The `root` hub

Every knowledge system has a guaranteed hub world named **`root`** that tracks things globally for the system. Org-wide conventions live there as ordinary versioned documents under a well-known prefix:

```text
mark://root/.well-known/demarkus/policy.md     ← strictness + required tags (see policy.md here)
mark://root/.well-known/demarkus/template.md   ← required per-world structure (the canonical layout)
```

`template.md` is the same layout the local soul seeds as `/project-template.md` — publish a copy of that to `root` (tailored as needed). `policy.md` is the example in this directory.

## How a joined agent consumes them

`/knowledge-join` registers the system so the publish tag-gate enforces on its writes (the same tag check as the local soul; tags travel in the tool call, so no broker access is needed for that). For the rest:

1. On first substantive work against a knowledge system, the agent fetches `mark://root/.well-known/demarkus/policy.md` and `template.md` and follows them — structure conventions are advisory by necessity (only the agent, via its MCP OAuth session, can reach the broker; a hook cannot).
2. The agent mirrors the policy's **enforced core** into local files the gate reads (`<slug>` is the MCP server name `/knowledge-join` registered, shown by `claude mcp list`):

   ```bash
   printf '%s\n' "block"      > ~/.demarkus/plugin-memory.strictness.<slug>     # from strictness:
   printf '%s\n' "category"   > ~/.demarkus/plugin-memory.require-tags.<slug>   # from require_tags:
   ```

   The org declares these once on `root`; each agent mirrors them; the local gate enforces them. `strictness:` sets the severity; `require_tags:` are tag axes the gate presence-checks (each satisfied by an `axis:value` tag, e.g. `category:project`).

## Why this shape

A shared catalog with many writers needs consistency conventions more than a personal soul does, and the org should set the bar centrally rather than each member re-deriving it. Hosting the policy and template on `root` means a single edit propagates to everyone, with no plugin release and no per-user setup — the conventions travel through the same knowledge system they govern.
