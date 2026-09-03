# Architecture Decision Records

Decision records binding the protocol, spec, and repo; git is canonical, and each ADR is mirrored verbatim into the project memory.

| ADR | Title | Status |
|-----|-------|--------|
| [0001](0001-broker-confidential-web-clients.md) | Broker confidential web-client registry | accepted 2026-06-11 |
| [0002](0002-okf-metadata-alignment.md) | Align store frontmatter with the Open Knowledge Format | accepted 2026-06-22 |
| [0003](0003-okf-type-default-on-publish.md) | Default OKF `type` on publish | accepted 2026-06-22 |
| [0004](0004-edge-semantics-rel-convention.md) | Edge semantics: provenance on every edge, typed relations via `rel-` metadata | accepted 2026-07-13 |
| [0005](0005-node-identity-default-port.md) | Node identity omits the default port | accepted 2026-08-18 |
| [0006](0006-postgres-backend-is-an-optional-build.md) | The Postgres backend is an optional build, not a dependency | superseded by 0010 |
| [0007](0007-sni-virtual-world-routing.md) | SNI selects a virtual Mark server | accepted 2026-08-22 |
| [0008](0008-gcs-root-cas-storage.md) | GCS world state commits through one root CAS | accepted 2026-08-22 |
| [0009](0009-license-split-core-agpl-plugins-mit.md) | AGPL for the core, MIT for plugins and satellites | accepted 2026-08-27 |
| [0010](0010-remove-postgres-backend.md) | Remove the Postgres backend | accepted 2026-08-27 |
