---
# The date and the 260 cap follow ../mocks/demarkus-memory/mark_fetch.md: above
# every fair summary seen (215), below the 352-character entry it must not copy.
type: regex
pattern: 'Recent journal\n+- 2026-10-11: [^\n]{1,260}\n'
match: contains
target: last_message
---
