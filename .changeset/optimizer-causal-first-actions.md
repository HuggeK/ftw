---
"ftw": patch
---

Optimizer backtests now compare one causal first action per non-overlapping measured interval. They no longer add overlapping horizon costs, exclude PV-curtail cases that replay cannot model, and label measured-data grid-cost repricing as a counterfactual rather than delivered savings.
