---
"ftw": patch
---

The box follows FTW Cloud to the instance that holds its home. When the service closes the uplink with 4421, the box dials the host named in the close reason once, only if it is `cloud.ftw.energy` or one label under it, and keeps that host with its link so a restart goes there first. It goes back to `cloud.ftw.energy` after a second redirect in a row, or after three failed dials over at least 30 s, so a shard's restart does not send it away. When the service closes with 4503 before a deploy, the box redials at a random moment within the spread the service names, at least 1 s, instead of all boxes coming back within half a second; a 4503 before `hello_ok` counts as a failure and grows the backoff. The box lists `shard` in `hello.caps`. The service still proves itself with the TLS certificate and the service key the box was built with, so a redirect grants nothing.
