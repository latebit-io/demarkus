---
layout: default
title: "From a soul to a knowledge system: the KS pattern with demarkus"
date: 2026-06-03
permalink: /blog/from-a-soul-to-a-knowledge-system/
---

# From a soul to a knowledge system

If you've used demarkus, it was probably as a soul, or as a personal wiki or journal. A little local server with all your secrets ;), one binary (0101) on whatever machine was lying around, holding your notes or your agent's memory. As a soul it remembers what you did last week so your agent doesn't start every session like it just woke up from a coma. As a wiki it's just your stuff, versioned, where you left it. Either way it's yours, and it's the right place to start.

The interesting question is what happens when it isn't just yours.

## You grow into it

Nobody starts at the top, and you shouldn't. The pattern grows the way the work does.

First it's the soul on your laptop. Then a teammate wants in, so you put a server on a VM and you both write to it, a auth token each, done. That's a perfectly good team knowledge base, and plenty of teams never need more than that.

Then it sprawls a bit. The infra notes don't belong in the same bucket as the API docs, so you stand up another VM, maybe a third. One server per concern, kept separate on purpose, linked with `mark://` when something in one genuinely depends on something in another. Without planning it you've built a little knowledge graph: a few servers that know how to point at each other. This is the [universe](/blog/creating-an-automated-knowledge-universe/) in miniature, and you can run it manually for a long time. They're just bookshelves.

What doesn't scale manually is the boring part. Once it's forty engineers/peoples with real turnover, and worlds that have to actually stay private, minting authtokens and emailing them around like it's 2009 stops being charming. Capability tokens are a lovely primitive and a miserable thing to hand out by "hand". That's the line where you want a full knowledge system, the enterprise or academic version of this pattern. So I stopped handing out tokens and built one.

## The broker / gateway

A knowledge system sticks a broker/gateway in front of the worlds.

The broker speaks OIDC. Your agent talks MCP to it over HTTPS, it checks you against your identity provider, mints a scoped token for the world you asked for, and forwards the request over QUIC. The worlds stay private behind it. Nobody carries a long-lived token; you log in with am OIDC provider like google.

The part I care about: none of this touched the protocol. The worlds are still dumb and fast. Capability auth still grants what you can do, not who you are. Identity lives out at the edge, in the broker and your IdP, and the core stays boring. Same bet demarkus always makes: keep the middle stupid, put the smarts somewhere you can throw away. To the agent it's invisible, the same fourteen tools as a server on localhost. The broker is plumbing, and agents shouldn't have to learn plumbing.

## Joining

Here's a developer joining their organizations system in Claude Code:

```
/plugin install demarkus-knowledge@demarkus
/knowledge-join https://knowledge.yourorg.com
```

That's it. The plugin checks the broker, registers it, and hands off to Claude Code's own OAuth. The first time your agent reaches the system you click through a browser sign-in. No token to copy, no file to edit. After that the whole org's knowledge is just there.

It picks up the house rules too. A system publishes its structure and tag policy to a `root` hub, and a joining agent reads it and follows it. Your conventions ride with the system instead of Confluence page nobody's opened since the offsite.

## Running one yourself

The other half is a template you fork. [`demarkus-knowledge-system-deploy`](https://github.com/latebit-io/demarkus-knowledge-system-deploy) brings up the whole thing on Kubernetes: broker, worlds, a `root` hub, GitOps and real secret management included. OpenTofu does the cluster and DNS, ArgoCD ships it from the repo, OpenBao holds the secrets. Fork it, point it at your domain, apply. GCP specific ATM :( 

`knowledge.demarkus.io` WIP, runs off that exact template, so it's the reference system and us eating our own cooking. Still rough in places, I won't pretend otherwise, but it's real, not a diagram with confident arrows.

## Souls still matter

None of this kills the soul. They're different layers in the same install. Your soul is the private scratchpad, your agent's working memory. The knowledge system is the shared thing the whole organization writes to. You think out loud on your soul, and when something's actually decided it goes up to the world. The soul keeps everything; the system keeps what you agreed to. Two plugins, `demarkus-memory` and `demarkus-knowledge`, sitting next to each other and not fighting.

## The point

If you need a knowledge system at scale, fork the template, point your agents at the broker, and let them do the one thing they're genuinely better at than us: spewing content, docs, and then actually maintaining them. 
