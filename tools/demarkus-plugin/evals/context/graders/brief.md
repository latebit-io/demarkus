---
# The whole reply is the soul-context template and nothing else; the primer
# has no Coverage lines, so nothing may follow Pick up where you left off.
type: regex
pattern: '^## [^\n]+\n\n### What[''’]s in flight\n[^\n#]+\n\n### Recent journal\n- [^\n]+\n\n### Pick up where you left off\n(?:(?:(?:- |\d+\. )[^\n]+\n?){1,2}|[^\n#-][^\n]*\n?)\s*$'
match: contains
target: last_message
---
