---
"ftw": patch
---

Release builds know FTW Cloud at `wss://cloud.ftw.energy` by the service's public key, so an owner can turn FTW Cloud on. Nothing changes until they do. The box checks every message from the service against that key and refuses a service that signs with any other.
