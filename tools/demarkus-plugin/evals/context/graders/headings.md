---
type: regex
pattern: '(^|\n)### What[''’]s in flight\n[\s\S]*\n### Recent journal\n[\s\S]*\n### Pick up where you left off\n'
match: contains
target: last_message
---
