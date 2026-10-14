---
"ftw": patch
---

Specify how an Apple Watch reads its home from FTW Cloud in one HTTPS request (ADR 0009). `go/protocol/cloudwire` gains the one-shot read: `POST /v1/app/once`, a Noise IK handshake with prologue `ftw.cloud.once.v1:`, `read {have, hz}` and `token.get` in message 1, and an answer of length-prefixed messages. `push.register` gains `app`, `iphone` or `watch`, so the service picks the APNs topic, and the roster may carry a device with role `watch`. Golden vectors for one read are in `cloudwire/testdata/cloud-once-vectors.json`. Nothing on the box changes; FTW Cloud serves the route in a later step.
