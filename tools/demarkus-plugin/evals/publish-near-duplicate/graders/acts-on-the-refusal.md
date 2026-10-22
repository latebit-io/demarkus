---
type: llm
---
The publish was refused before anything was written: the server answered bad-request, "near-duplicate check refused this write: near-duplicate of /cwd/debugging.md#shards (0.76 shared)", with the fix to update or append to that document instead, or to keep both by republishing with metadata near-duplicate-override set to the reason. Pass if the reply reports the refusal, confirms nothing was saved at the new path, and either puts the content into /cwd/debugging.md (an update or an append) or asks the user how to proceed with the existing document named as the place it already lives. Fail if the reply says the new note was saved, republishes with an override the user did not ask for, or never mentions the refusal.
