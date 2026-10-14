---
"ftw": patch
---

An owner's phone can add an Apple Watch as a device of its own (ADR 0009). The phone asks for a watch code over its session, with a passkey step-up on that request; the code binds to the phone and leaves a pending phone code alone. The box stores the watch with role `watch` and the phone as its parent. A watch holds the watch role's scopes that its phone holds at that moment, so demoting the phone narrows the watch at once. Removing a phone removes its watches; removing a watch touches nothing else. A watch never gets a stream session. The box's page and `GET /api/app-link/devices` list each watch under its phone, and FTW Cloud's roster carries it with the scopes the box derives. The box does not offer the watch path yet: `cmd.exchange` stays unsaid until the exchange socket lands.
