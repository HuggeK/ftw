---
"ftw": patch
---

Keep a safe plan when a later forecast cannot meet the site limits. The reserve planner prepares the battery for future peaks and reports why it shortened a plan, while Core still checks every planned action.

Retain valid intervals before corrupt future forecast values, use the reserve when the DP grid exceeds its memory budget, and require fresh battery charge readings before planning from live telemetry.
