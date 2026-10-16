---
layout: default
title: Install a Knowledge System
permalink: /install/stack/
---

# Install a knowledge system

The `demarkus-knowledge-system` Helm chart installs many worlds, OIDC-backed MCP
gateways, and an optional [reading room](/library/). Declare each world once in
one values file.

The former `install-stack.sh` appliance has been retired.

## What it runs

| Component | Role |
|---|---|
| `demarkus-knowledge` | World storage, identity, MCP gateways, and cross-world graph |
| `demarkus-library` | Web reading room and optional AI librarian |

The knowledge deployment runs at least two replicas. Each world keeps its history
in a GCS bucket; the server initializes an empty world on first start.

## Prerequisites

- Kubernetes, GCS buckets, and workload identity
- An OIDC provider and registered client
- Ingress and TLS for the gateway and library

Use the chart's [example values](https://github.com/latebit-io/demarkus/tree/main/deploy/helm/demarkus-knowledge-system)
to configure worlds, buckets, login, and the library's OAuth client. Then install:

```bash
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --create-namespace --values knowledge-system.yaml
```

Users join the configured gateway:

```text
/knowledge-join https://knowledge.example.com
```

## Smaller setup

For one world on a Linux host, install the server with a read-only reading room:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  sudo bash -s -- --domain docs.example.com --root /srv/site --with-library
```

This uses direct QUIC and capability tokens. See [team setup](/scenarios/team/)
for shared writes, or [knowledge-system architecture](/scenarios/knowledge-system/)
for the multi-world deployment.
