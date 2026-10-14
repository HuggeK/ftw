---
"ftw": patch
---

An app command retried in a new session no longer acts twice. Each app session kept its own list of command ids, so when a phone lost its connection mid-command and sent the command again, the box acted again. The box now keeps one list for all sessions, keyed by the device and its command id, and holds each id for 24 hours of uptime, as the protocol says. Another device's command with the same id still counts as new.
