---
layout: default
title: Getting Started
permalink: /getting-started/
---

# Getting Started

Quick path to running Demarkus locally and publishing this site with GitHub Pages.

## 1. Build binaries

```bash
make client
make server
```

## 2. Run a local server

```bash
./server/bin/demarkus-server -root ./content -port 6309
```

## 3. Fetch a document

```bash
./client/bin/demarkus mark://localhost:6309/index.md
```

## 4. Deploy this website

1. Open repository **Settings -> Pages**.
2. Set **Source** to **GitHub Actions**.
3. Push to the `pages` branch.
4. Wait for the `Deploy Pages` workflow to finish.
