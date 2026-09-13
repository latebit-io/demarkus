# Document Graph

> Exported: GOLDEN
> Nodes: 3
> Edges: 3

## Nodes

| URL | Title | Status | Links |
|-----|-------|--------|-------|
| [mark://team-a.team-a.svc.cluster.local/docs/a.md](mark://team-a.team-a.svc.cluster.local/docs/a.md) | Applications | ok | 2 |
| [mark://team-a.team-a.svc.cluster.local/docs/b.md](mark://team-a.team-a.svc.cluster.local/docs/b.md) | B doc | ok | 0 |
| [mark://team-a.team-a.svc.cluster.local/index.md](mark://team-a.team-a.svc.cluster.local/index.md) | Team A hub | ok | 1 |

## Edges

| From | To | Rel | Label | Anchor | Count |
|------|----|-----|-------|--------|-------|
| mark://team-a.team-a.svc.cluster.local/docs/a.md | mark://team-a.team-a.svc.cluster.local/docs/b.md |  | B doc | links | 2 |
| mark://team-a.team-a.svc.cluster.local/docs/b.md | mark://team-a.team-a.svc.cluster.local/docs/a.md | supersedes |  |  | 1 |
| mark://team-a.team-a.svc.cluster.local/index.md | mark://team-a.team-a.svc.cluster.local/docs/a.md |  | Applications | services | 1 |
