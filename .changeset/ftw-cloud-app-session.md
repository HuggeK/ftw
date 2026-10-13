---
"ftw": patch
---

Let phones read their home from FTW Cloud. While FTW Cloud is on, the box tells every paired phone, over its own encrypted session, the key it signs with and the names behind its anonymous ids, so a phone can check what the service forwards. Boxes that can run the uplink say so to the app. `POST` and `DELETE /api/cloud/stats` record and withdraw an owner's consent to statistics; nothing is sent for it yet.
