# Memory Template

The canonical layout for this memory. Agents route new knowledge to these documents instead of inventing parallel structures; `/index.md` is the hub and links everything worth finding.

## Routes

- A decision worth remembering: `/adr/<NNNN>-<title>.md`, numbered sequentially, with a `Decision` type in metadata. Add a row to the hub's Decisions section.
- A lesson from a bug or investigation: `/debugging.md` (append a uniquely titled section). Often the highest recall value.
- A pattern or convention learned: `/patterns.md`.
- Session progress and working notes: `/journal/<YYYY-MM-DD>.md`, one document per day, appended through the day.
- A plan for a larger piece of work: `/plans/<name>.md`, linked from the hub's Active section.
- Reflections and ideas that fit nowhere else: `/thoughts.md`.

## Rules

- Keep `/index.md` current: it is the discovery backstop for anything a catalog lookup cannot surface.
- Tag every publish (`tags`, `importance` in the metadata object); prefer one well-tagged document over scattered fragments.
- Capture the non-obvious (why a decision was made, a gotcha); skip trivia and anything derivable from code or history.
- Never store secrets or credentials.
