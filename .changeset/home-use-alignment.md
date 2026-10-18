---
"ftw": patch
---

Calculate Home use from timestamped charger session energy where available and integrate other sources over the same period. Adapt the period to source reporting and show the estimated mean, period and age. Cloud polls do not refresh source time. Retain signed daily energy only for bounded balance failures that recover, while preserving raw quality, offline gaps and control inputs.
