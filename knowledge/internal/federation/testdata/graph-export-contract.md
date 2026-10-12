# Document Graph

> Nodes: 3
> Edges: 3

## Nodes

| URL | Title | Status | Links |
|-----|-------|--------|-------|
| [mark://alpha/docs/a.md](mark://alpha/docs/a.md) | Applications | ok | 2 |
| [mark://alpha/docs/b.md](mark://alpha/docs/b.md) | B doc | ok | 0 |
| [mark://alpha/index.md](mark://alpha/index.md) | Team A hub | ok | 1 |

## Edges

| From | To | Rel | Label | Anchor | Count |
|------|----|-----|-------|--------|-------|
| mark://alpha/docs/a.md | mark://alpha/docs/b.md |  | B doc | links | 2 |
| mark://alpha/docs/b.md | mark://alpha/docs/a.md | supersedes |  |  | 1 |
| mark://alpha/index.md | mark://alpha/docs/a.md |  | Applications | services | 1 |
