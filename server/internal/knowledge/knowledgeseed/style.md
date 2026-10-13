# Style Guide

How documents in this knowledge system are written so agents and people can find, fetch and link them: metadata out of band, one name per document, stable headings, hubs that are only links.

## Metadata

Metadata travels in the request, never in the body. Set `tags` (comma-separated subjects drawn from the content), `importance` (0 to 1, with 0.8 and above reserved for hubs, architecture and accepted decisions), `type` (the document's kind: Reference, Decision, Architecture, Plan, Journal, Guide) and `description` (one sentence, the same sentence that sits under the H1). The policy at `/.well-known/demarkus/policy.md` says which tag axes and fields are required and which values an axis allows.

## Documents

A document opens with one `# H1`, its name, and one summary sentence directly under it saying what the subject is. Headings below it are names alone and stay unique: they are the anchors inbound links depend on, so status (a date, a PR number, DONE) goes on the first line under the heading, never in it. A document that outgrows one fetch (8 KB) splits into a hub plus topic files.

## Hubs

A hub (`index.md` at any depth, or any link page) is links plus one line each. No content, status, dates or summaries copied from children; the child is the single source and links back to its hub. About forty outbound documents, then a second-level hub.

## Punctuation

No em dashes anywhere, prose, headings or examples; use a comma, colon, semicolon or parentheses.

## Enforced rules

The section below is rendered from the style gate's rule table and kept current by the server; edit the prose above, not this list. A publish finding opens with the rule id it cites.

