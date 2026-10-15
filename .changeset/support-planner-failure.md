---
"ftw": patch
---

Include the latest failed planner inputs, worker request and response, and current charging state in support packages. Retain the latest failure across restarts with a durable SQLite commit and report storage errors. Keep failure evidence distinct from accepted plans and redact credentials before export.
