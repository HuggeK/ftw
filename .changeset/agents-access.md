---
"ftw": minor
---

The box enforces agent access (`docs/agents.md`): an owner can let an assistant, Sourceful support for one ticket or the release check reach the box through FTW Cloud's tunnel, never the relay. Each grant has scopes, an op allowlist, caps on hold length, power and rate, and a wall-clock expiry that fails closed without NTP; revoke and expiry bite at the next call. Holds end by themselves; durable changes wait as requests until an owner confirms them with a phone's approval key or on the box page with the house password. The box keeps an audit log of who did what, serves log excerpts and configuration redacted on the box, and pushes `agent.added` and `agent.request` to owners.
