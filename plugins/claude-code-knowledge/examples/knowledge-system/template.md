# Knowledge System Template

Audience: anyone creating or extending a world in this knowledge system. The canonical per-world layout; agents route new documents to these paths instead of inventing parallel structures. Published once at `mark://root/.well-known/demarkus/template.md`; joined agents fetch it with `force: true` and follow it. Writing rules live in the [style guide](/.well-known/demarkus/style.md); tag rules in the [policy](/.well-known/demarkus/policy.md).

## World layout

- `/index.md`: the world hub. Links to every top-level section and project, one line each. Untyped.
- `/.well-known/demarkus/world.md`: the world descriptor (team, domain, partition role, autonomy ceiling). Per-world, owner-published.
- `/projects/<slug>/`: one subtree per project, laid out as below. `<slug>` is the repository name, lowercase.
- Shared, cross-project documents sit at the top level next to `/index.md` and are linked from it.

## Project layout

Each `/projects/<slug>/` subtree:

- `index.md`: project hub, links every document below. Untyped, `category:navigation`.
- `architecture.md`: system design, module boundaries, key decisions. Type `Architecture`.
- `adr/index.md` and `adr/<NNNN>-<title>.md`: decision records, numbered sequentially. Type `Decision`; `rel-supersedes` metadata when one replaces another.
- `guidelines.md`: hard code-quality rules. Type `Guide`, `category:standards`.
- `patterns.md`: implementation, testing, and workflow patterns. Type `Guide`.
- `debugging.md`: lessons from bugs and investigations, one uniquely titled section each. Type `Guide`.
- `roadmap.md`: what is next, active, done, and deliberately not prioritized. Type `Plan`.
- `plans/<name>.md`: one document per larger piece of work, linked from the project hub. Type `Plan`.

## Hubs

Every hub (`/index.md`, project `index.md`, `adr/index.md`, any link page) is a link page: links plus one line each, no content copied from children, each child links back, about 40 outbound documents before a second-level hub. A document that outgrows one fetch splits into a hub plus topic files. Full rule in the style guide.

## Metadata

- Every publish sets `tags` (subjects from the content plus the policy's required axes, at minimum `category:<value>`), `importance` (0 to 1; 0.8 and above for hubs, architecture, and key decisions), and `type` where the layout names one.
- Metadata travels in the publish `metadata` object, never in the body.
- Never set `retention` on curated documents.
