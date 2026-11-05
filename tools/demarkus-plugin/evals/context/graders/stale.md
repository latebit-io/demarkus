---
# The fixture's entry is always over a week old: the first suggestion says
# so, inside the template, so the reply's shape holds whatever today is.
type: regex
flags: i
pattern: '### Pick up where you left off\n[^\n]*(?:\b(?:weeks?|days?|months?) (?:ago|old)\b|\bover a (?:week|month)\b|\bstale\b)'
match: contains
target: last_message
---
