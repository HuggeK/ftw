---
"ftw": patch
---

Keep the newest MPC replan when solves finish out of order. Each request now
keeps its mode and reason together, and an older result cannot replace a plan
started after it.
