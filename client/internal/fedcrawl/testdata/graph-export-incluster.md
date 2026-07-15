# Document Graph

> Exported: GOLDEN
> Nodes: 3
> Edges: 3

## Nodes

| URL | Title | Status | Links |
|-----|-------|--------|-------|
| [mark://team-a.team-a.svc.cluster.local:6309/a.md](mark://team-a.team-a.svc.cluster.local:6309/a.md) | Applications | ok | 1 |
| [mark://team-a.team-a.svc.cluster.local:6309/b.md](mark://team-a.team-a.svc.cluster.local:6309/b.md) | B doc | ok | 0 |
| [mark://team-a.team-a.svc.cluster.local:6309/index.md](mark://team-a.team-a.svc.cluster.local:6309/index.md) | Team A hub | ok | 1 |

## Edges

| From | To | Rel | Label | Anchor | Count |
|------|----|-----|-------|--------|-------|
| mark://team-a.team-a.svc.cluster.local:6309/a.md | mark://team-a.team-a.svc.cluster.local:6309/b.md |  | B doc | links | 1 |
| mark://team-a.team-a.svc.cluster.local:6309/b.md | mark://team-a.team-a.svc.cluster.local:6309/a.md | supersedes |  |  | 1 |
| mark://team-a.team-a.svc.cluster.local:6309/index.md | mark://team-a.team-a.svc.cluster.local:6309/a.md |  | Applications | services | 1 |
