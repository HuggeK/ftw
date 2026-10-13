---
"ftw": patch
---

Add the FTW Cloud uplink, off unless an owner turns it on. `GET /api/cloud` shows the grant and the bytes sent in the last 24 hours, `GET /api/cloud/preview` shows the exact bytes that leave the house, and `POST` and `DELETE /api/cloud/grant` turn it on and off. Release builds name no FTW Cloud service yet, so the box refuses a grant and never dials. Backups no longer include `cloud.key`.
