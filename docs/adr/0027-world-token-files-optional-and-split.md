# ADR 0027: World token files are optional at open and split into runtime and static

Status: proposed (2026-09-28).

## Context

A knowledge world refused to open without its tokens file, and the file was
one Secret the broker appends to at runtime. No reconciler can own that Secret
(a sync would reset the broker's entries), so the Helm chart created it with a
pre-install Job: an extra image on the cluster's allowlist, RBAC over Secrets,
a kubectl pinned to the API server's minor, and hook ordering under Argo CD.
The Job also minted the agent's admin token, so removing it needed another
home for that token ([#517](https://github.com/latebit-io/demarkus/issues/517)).

## Decision

- A world's token files are optional at open when the server says so
  (`worldruntime.Config.OptionalTokensFiles`, always set by the knowledge
  server). A missing file contributes no tokens; the store is the union of
  the files present on disk, on the first open and on every reload. The
  directory watcher loads a file that appears later. A file that vanishes
  revokes its entries on the next reload. The single-host
  `demarkus-server` stays strict: its worlds may hold read tokens, and a
  mistyped path must not make a private world public.
- Each world has two files: `auth.tokensFile`, the runtime file a broker
  appends to, and `auth.staticTokensFile`, operator-owned entries such as an
  agent's publish token. `auth.Source` merges them; a hash in both is a load
  error, and the config refuses a path used twice across worlds or files.
- The chart projects both Secrets, optional, into one directory per world:
  `<name>-tokens` (broker-created) and `<name>-static-tokens` (operator
  supplied, hashes only, safe in git). The bootstrap Job stays on by default
  for existing installs; a GitOps install turns it off and supplies the
  static Secret plus the raw `<hub>-token-values` the agent reads.

## Consequences

- An Argo CD install renders no Job, no extra image and no Secret RBAC, and
  two syncs in a row leave the broker's entries untouched.
- The agent's token is static: rotation is a new hash in the static Secret
  and a new raw value in its Secret, hot-reloaded on the server side.
- A world without any token Secret is silently read-only until one lands;
  the open logs one Info line per missing file.
- `configwatch.Watcher` takes a target set in one directory, so both files
  share one watcher and one debounce in every server.
