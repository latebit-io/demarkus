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

## Source observations

```json
[{"url":"mark://team-a.team-a.svc.cluster.local/docs/a.md","observation":{"source":"mark://team-a.team-a.svc.cluster.local/docs/a.md","view":"federation","revision":2,"etag":"etag-/docs/a.md","complete":true,"observed_at":"2026-09-13T00:00:00Z","attempted_at":"2026-09-13T00:00:00Z"}},{"url":"mark://team-a.team-a.svc.cluster.local/docs/b.md","observation":{"source":"mark://team-a.team-a.svc.cluster.local/docs/b.md","view":"federation","revision":3,"etag":"etag-/docs/b.md","complete":true,"observed_at":"2026-09-13T00:00:00Z","attempted_at":"2026-09-13T00:00:00Z"}},{"url":"mark://team-a.team-a.svc.cluster.local/index.md","observation":{"source":"mark://team-a.team-a.svc.cluster.local/index.md","view":"federation","revision":4,"etag":"etag-/index.md","complete":true,"observed_at":"2026-09-13T00:00:00Z","attempted_at":"2026-09-13T00:00:00Z"}}]
```
