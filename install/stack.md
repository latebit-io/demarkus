---
layout: default
title: Install a Knowledge System
permalink: /install/stack/
---

# Install a knowledge system

One Helm chart, `demarkus-knowledge-system`, installs the worlds, single
sign-on, the MCP gateways, and an optional [library](/library/). Declare each
world once in one values file.

## What runs

| Component | Role |
|---|---|
| `demarkus-knowledge` | Worlds, OIDC login, knowledge and memory gateways, cross-world graph |
| `demarkus-library` | Web reading room, with an optional AI agent |

- **Two or more replicas**, stateless. Each world keeps its history in its own GCS bucket; writes commit by compare-and-swap.
- **New worlds on first start.** An empty bucket becomes a world, seeded with the chart's initial policy.
- **Background work** (sweeper, cross-world graph) runs on one replica under a Kubernetes Lease.

## Prerequisites

- Kubernetes with workload identity, and one GCS bucket per world
- An OIDC provider and a registered client
- Ingress and TLS for the gateway and the library

## Install

Start from the chart's
[example values](https://github.com/latebit-io/demarkus/tree/main/deploy/helm/demarkus-knowledge-system),
then:

```bash
helm upgrade --install demarkus oci://ghcr.io/latebit-io/charts/demarkus-knowledge-system \
  --namespace demarkus --create-namespace --values knowledge-system.yaml
```

Publish your policy, template, and style guide to
`mark://root/.well-known/demarkus/`. Joining agents read them, so your
conventions need no separate onboarding doc.

Users join:

```text
/knowledge-join https://knowledge.example.com
```

## Hosted memory

Give the gateway a second hostname (`broker.memory.publicURL`) and it serves
hosted memory: one private world per signed-in person, provisioned on first use
(`static`, `allowlisted`, or `open`). Users connect any MCP host to
`https://memory.example.com/mcp`. [Memory →](/memory/#remote-and-hosted-memory)

## References

- [Chart README](https://github.com/latebit-io/demarkus/tree/main/deploy/helm/demarkus-knowledge-server): every value, rate limits, HA
- [Reference deployment](https://github.com/latebit-io/demarkus-knowledge-system-deploy): GKE and GitOps
- [Local harness](https://github.com/latebit-io/demarkus/tree/main/deploy/kind): kind with a mock OIDC and fake GCS

## Smaller

One world on one Linux host, with a read-only reading room, no single sign-on:

```bash
curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | \
  sudo bash -s -- --domain docs.example.com --root /srv/site --with-library
```

See the [team server guide](/scenarios/team/) for shared writes with tokens.
