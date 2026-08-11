---
"ftw": patch
---

Bound each optimizer request to one worker deadline so expired queued work no longer blocks newer plans or resets its budget between solver phases.
