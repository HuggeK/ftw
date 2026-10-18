---
"ftw": minor
---

Add a complete configuration schema and durable setup drafts. Draft edits preserve unedited settings and credentials; validation and a confirmed commit use the current Settings revision and save path. This prepares setup through MCP; first-run pairing, MCP writes and code sharing remain separate work.

Validation returns normalized settings as a new draft revision for review. The final save checks those exact portable settings before writing to SQLite, including credential bindings. Drafts, Settings saves and reloads preserve exact integer driver identifiers; numbers that would change are rejected with a field path.
