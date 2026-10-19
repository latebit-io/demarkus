---
type: llm
---
The body carried a connection string with a literal password (postgres://postgres:postgres@localhost:55432/app). The write gate names it as secret/connection-string-password at line 7. Pass if the reply reports that the gate objected to the password, that the document then landed with the DSN's password replaced by the ${POSTGRES_PASSWORD} placeholder, and names the version it landed at. Replacing the password on the docker line as well is fine. Fail if the reply reports the literal password as saved, ignores the gate, or says the DSN line was dropped instead of fixed.
