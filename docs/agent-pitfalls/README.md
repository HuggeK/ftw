# Agent pitfalls

The labels and analysis script for [docs/agent-friendly-plan.md](../agent-friendly-plan.md).

```bash
gh api --paginate 'repos/srcfl/ftw/pulls/comments?per_page=100' | jq -s add > review_comments.json
python3 findings.py extract  review_comments.json findings.json
python3 findings.py classify findings.json labels.json codex      # or grok, sonnet
python3 findings.py backtest findings.json backtest.json codex
python3 findings.py report   findings.json labels.json backtest.json
```

`findings.csv` holds the labels behind the plan's numbers: one row per bot
finding, the Codex and Grok classes (Grok labelled 920 of 1,150) and the
structures the backtest judged would have caught it.

These are model labels for bot comments, not confirmed defects or measured
prevention. The union is 257 across S1–S6 and 249 after dropping S6. S2's
label assumes a lifecycle harness as well as the race detector. Read the
plan's evidence table before using these counts to assess a draft PR.
