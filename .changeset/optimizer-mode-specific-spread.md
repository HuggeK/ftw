---
"ftw": patch
---

Fix two optimizer result rules. The minimum arbitrage spread now applies only in arbitrage modes, so self-consumption and cheap-charge plans can serve load without an unrelated discharge cost. CVXPY plans that stop at the solver time limit now fail unless the solver reports an accepted optimal status.
