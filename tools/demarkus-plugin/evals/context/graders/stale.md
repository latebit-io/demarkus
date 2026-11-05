---
# The fixture's entry is always over a week old: the first suggestion says
# so, inside the template, so the reply's shape holds whatever today is.
type: regex
flags: i
pattern: '### Pick up where you left off\n[^\n]*(?:2026-10-11|\b(?:weeks?|days?|months?) (?:ago|old)\b)'
match: contains
target: last_message
---
