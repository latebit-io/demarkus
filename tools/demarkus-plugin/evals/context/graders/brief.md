---
# The whole reply is the soul-context template and nothing else; the primer
# has no Coverage lines, so nothing may follow Pick up where you left off's one
# or two suggestion lines, listed or prose.
type: regex
pattern: '^## [^\n]+\n\n### What[''’]s in flight\n[^\n#]+\n\n### Recent journal\n- [^\n]+\n\n### Pick up where you left off\n(?:[^\n#][^\n]*\n*){1,2}\s*$'
match: contains
target: last_message
---
