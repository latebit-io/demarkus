#!/usr/bin/env bash
#
# Generates a set of interconnected markdown documents for testing
# the graph crawler. Publishes them to a running Demarkus server.
#
# Usage:
#   ./scripts/seed-test-docs.sh [host:port] [auth-token]
#
# Defaults:
#   host:port  = localhost:6309
#   auth-token = $DEMARKUS_AUTH (or empty)

set -euo pipefail

HOST="${1:-localhost:6309}"
TOKEN="${2:-${DEMARKUS_AUTH:-}}"
DEMARKUS="./client/bin/demarkus"

if [[ ! -x "$DEMARKUS" ]]; then
  echo "Building client..."
  make client
fi

publish() {
  local path="$1"
  local body="$2"
  local args=(-X PUBLISH -insecure -body "$body")
  if [[ -n "$TOKEN" ]]; then
    args+=(-auth "$TOKEN")
  fi
  echo "  PUBLISH $path"
  "$DEMARKUS" "${args[@]}" "mark://${HOST}${path}" 2>&1 | head -1
}

echo "Seeding test documents on ${HOST}..."
echo

# ── Hub pages ──────────────────────────────────────────────────

publish "/index.md" "# Knowledge Base

Welcome to the test knowledge base.

## Topics

- [Computer Science](cs/index.md)
- [Philosophy](philosophy/index.md)
- [History](history/index.md)
- [About this site](about.md)
- [Getting Started](getting-started.md)
"

publish "/cs/index.md" "# Computer Science

An overview of computer science topics.

## Areas

- [Algorithms](algorithms.md)
- [Data Structures](data-structures.md)
- [Networking](networking.md)
- [Operating Systems](operating-systems.md)
- [Programming Languages](languages.md)

See also: [Philosophy of Computing](../philosophy/computing.md)
"

publish "/philosophy/index.md" "# Philosophy

Exploring fundamental questions.

## Topics

- [Epistemology](epistemology.md)
- [Ethics](ethics.md)
- [Philosophy of Computing](computing.md)
- [Logic](logic.md)

See also: [Computer Science](../cs/index.md)
"

publish "/history/index.md" "# History

Key moments in technology history.

## Eras

- [Early Computing](early-computing.md)
- [The Internet](internet.md)
- [Open Source Movement](open-source.md)

See also: [Computer Science](../cs/index.md) | [Philosophy](../philosophy/index.md)
"

# ── Computer Science documents ─────────────────────────────────

publish "/cs/algorithms.md" "# Algorithms

The study of computational procedures.

## Key Topics

- Sorting and searching
- Graph algorithms
- Dynamic programming

## Related

- [Data Structures](data-structures.md) — algorithms operate on data structures
- [Programming Languages](languages.md) — algorithms are expressed in languages
- [Logic](../philosophy/logic.md) — formal foundations
"

publish "/cs/data-structures.md" "# Data Structures

Organizing and storing data efficiently.

## Common Structures

- Arrays and linked lists
- Trees and graphs
- Hash tables
- Heaps and priority queues

## Related

- [Algorithms](algorithms.md) — algorithms manipulate data structures
- [Operating Systems](operating-systems.md) — OS internals use specialized structures
"

publish "/cs/networking.md" "# Networking

How computers communicate.

## Layers

- Physical and data link
- Network and transport
- Application protocols

## Related

- [The Internet](../history/internet.md) — history of networking
- [Operating Systems](operating-systems.md) — network stacks live in the OS
- [Algorithms](algorithms.md) — routing algorithms
"

publish "/cs/operating-systems.md" "# Operating Systems

Managing hardware and software resources.

## Core Concepts

- Process management
- Memory management
- File systems
- Device drivers

## Related

- [Data Structures](data-structures.md) — internal OS data structures
- [Networking](networking.md) — network subsystem
- [Early Computing](../history/early-computing.md) — OS history
"

publish "/cs/languages.md" "# Programming Languages

Tools for expressing computation.

## Paradigms

- Imperative and procedural
- Object-oriented
- Functional
- Logic programming

## Related

- [Algorithms](algorithms.md) — languages express algorithms
- [Philosophy of Computing](../philosophy/computing.md) — what is computation?
- [Open Source](../history/open-source.md) — languages and community
"

# ── Philosophy documents ───────────────────────────────────────

publish "/philosophy/epistemology.md" "# Epistemology

The study of knowledge.

## Key Questions

- What is knowledge?
- How is knowledge acquired?
- What are the limits of knowledge?

## Related

- [Logic](logic.md) — formal reasoning
- [Ethics](ethics.md) — knowledge and moral responsibility
"

publish "/philosophy/ethics.md" "# Ethics

The study of moral principles.

## Frameworks

- Consequentialism
- Deontology
- Virtue ethics

## Related

- [Epistemology](epistemology.md) — knowing right from wrong
- [Philosophy of Computing](computing.md) — AI ethics
- [Open Source](../history/open-source.md) — ethics of sharing
"

publish "/philosophy/computing.md" "# Philosophy of Computing

What is computation? What can be computed?

## Key Ideas

- Turing machines and computability
- The halting problem
- Artificial intelligence and consciousness
- Information theory

## Related

- [Computer Science](../cs/index.md) — the practice
- [Epistemology](epistemology.md) — knowledge and computation
- [Logic](logic.md) — formal systems
- [Programming Languages](../cs/languages.md) — expressing computation
"

publish "/philosophy/logic.md" "# Logic

The study of valid reasoning.

## Branches

- Propositional logic
- Predicate logic
- Modal logic
- Fuzzy logic

## Related

- [Epistemology](epistemology.md) — logic and knowledge
- [Algorithms](../cs/algorithms.md) — logic in computation
- [Philosophy of Computing](computing.md) — formal systems
"

# ── History documents ──────────────────────────────────────────

publish "/history/early-computing.md" "# Early Computing

From abacus to transistor.

## Milestones

- Charles Babbage's Analytical Engine
- Alan Turing's universal machine
- ENIAC and the first electronic computers
- The transistor revolution

## Related

- [The Internet](internet.md) — what came next
- [Operating Systems](../cs/operating-systems.md) — early OS development
- [Philosophy of Computing](../philosophy/computing.md) — Turing's legacy
"

publish "/history/internet.md" "# The Internet

A global network of networks.

## Timeline

- ARPANET (1969)
- TCP/IP standardization (1983)
- The World Wide Web (1991)
- The modern web

## Related

- [Networking](../cs/networking.md) — technical foundations
- [Open Source](open-source.md) — the internet enabled open source
- [Early Computing](early-computing.md) — what came before
"

publish "/history/open-source.md" "# Open Source Movement

Software freedom and collaboration.

## Key Moments

- GNU Project (1983)
- Linux kernel (1991)
- Open Source Initiative (1998)
- GitHub era (2008+)

## Related

- [The Internet](internet.md) — enabling infrastructure
- [Programming Languages](../cs/languages.md) — tools of the trade
- [Ethics](../philosophy/ethics.md) — the ethics of sharing
- [Early Computing](early-computing.md) — historical context
"

echo
echo "Done! Seeded documents on ${HOST}."
echo
echo "Test the graph:"
echo "  $DEMARKUS graph -insecure -depth 1 mark://${HOST}/index.md"
echo "  $DEMARKUS graph -insecure -depth 2 mark://${HOST}/index.md"
echo "  $DEMARKUS graph -insecure -depth 3 mark://${HOST}/cs/index.md"
