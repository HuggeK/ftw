---
"ftw": patch
---

Recover saved household load learning when the raw interval archive proves the old clock-jump collection bug. Keep solar learning, charging goals and the raw record. A permanently rejected archived observation no longer blocks later valid training; disk, worker and model failures still retry.
