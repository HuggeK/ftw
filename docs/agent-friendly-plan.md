# Making FTW safe for agents to change (proposed)

Status: proposed. Every step below is a separate pull request. Steps 1, 2
and 4 exist as drafts. The rest are designs. Nothing has merged.

Agents write most of FTW. 919 of the 2,250 commits on `master` carry a
Claude co-author trailer, and 346 of the 1,415 pull requests come from
`agent/`, `codex/`, `claude/` or `cursor/` branches. Both counts are lower
bounds, since many agent PRs carry no marker. Review bots then raised
1,150 inline findings on 573 pull requests. These are bot comments, not
1,150 confirmed, distinct defects. 760 of the 1,735
conventional commits on `master` are `fix` commits.

Agents copy the nearest example and take the shortest path that compiles.
If the repository lets a wrong change look right from inside one file, an
agent will make it. This plan changes the repository so that the wrong
change fails to compile, fails CI, or cannot be written.

## How the method works

The method comes from @poteto's pstack skills (`/correct`, `/architect`,
`/create-verification-skill`). It treats repeated agent mistakes as defects
in the repository, not in the agent.

1. **Collect the corrections.** Read what reviewers and the owner keep
   pointing out: review comments, fix commits, reverts, issues, and the
   rules in AGENTS.md. Here the main source is the 1,150 bot findings, plus
   the 71 issues labelled `bug` or `release-blocker`.
2. **Group them into classes.** One finding is an accident. A class is a
   mistake that keeps coming back. A class counts once it has happened
   twice.
3. **Fix each class at the highest level that works.** In order:
   architecture (one owner, one way to do the task, so the wrong way does
   not exist), then types (the bad state cannot be written), then a lint or
   CI check whose error names the fix, then a behaviour test. Docs come last,
   because nothing fails when an agent skips them.
4. **Sketch the shape before the code.** For an architectural fix, write
   the types and signatures first and compare at least two shapes
   (`/architect`).
5. **Answer open questions by building, not by asking.** Whether the race
   detector finds anything is a fact you can observe, so the prototype runs
   it.
6. **Backtest.** For each proposed structure, count how many historical
   mistakes it would have caught, then rank the structures by that count.
   A structure that would have caught nothing does not earn its cost.
7. **Prove each check on a real past mistake.** A new check must fail on a
   commit that actually had the bug, and pass on today's `master`.
8. **Give agents a way to prove behaviour.** A verification skill starts
   the real app and drives it, so "it works" means a run, not a reading of
   the code.
9. **Keep a rule table.** AGENTS.md pairs each rule with the thing that
   enforces it. A rule with no enforcer is the next candidate for steps 3–7.

## The data

`docs/agent-pitfalls/findings.py` extracts the findings, classifies them,
estimates which structures would prevent them and prints the report. Classification and
backtest are LLM judgements, made in batches of 40 from excerpts of up to
450 characters. The judge did not compile or run each historical change.
`docs/agent-pitfalls/findings.csv` holds every label, so a reviewer can
spot-check any row.

Codex labelled all 1,150 findings, and Grok labelled 920 of them
independently as a second rater. The two agreed on 78.9 % of those 920
(726). Both raters put the same four classes in their top five. Agreement
measures consistency between these labels; it does not establish that the
comments describe real defects or that the proposed fixes prevent them.
The sample can contain duplicate reports, false alarms and findings that
reviewers later resolved. It excludes mistakes that these bots missed.

| Class | Findings | PRs | P1 or High |
|---|---:|---:|---:|
| concurrency-lifecycle: races, ordering, cancellation, restart, retry | 208 | 149 | 59 |
| incomplete-wiring: new behaviour missing on some entry point, or a refactor drops a path | 197 | 145 | 70 |
| legacy-state: stored config, DB rows or YAML seeds on existing installs ignored | 102 | 79 | 45 |
| unknown-as-value: missing, stale or failed data used as 0, default or last value | 101 | 83 | 32 |
| duplicated-truth: one rule in several places, or docs contradicting code | 95 | 71 | 8 |
| power-units-sign | 77 | 64 | 34 |
| release-ci | 77 | 67 | 32 |
| ui-contract | 75 | 58 | 11 |
| security | 73 | 56 | 31 |
| silent-failure | 56 | 47 | 12 |
| other | 89 | 71 | 16 |

The bugs that escaped to issues (71) follow the same top classes:
concurrency-lifecycle 15, security 12, legacy-state 10, unknown-as-value 8.

## The top five pitfalls

**1. Concurrency and lifecycle (208).** Examples: OCPP not restarted on a
config hot-reload (#979), a stale driver-restart callback acting on an old
config copy (#1628), and Lua VM state lost when reprobing fails (#982).
CI runs `go test` without `-race`. Agents often report "race tests pass"
from local runs, yet #787 and #789 merged with data races in test fakes.
- **Level: CI check now, architecture later.** The race detector is the
  cheapest check that fails on real past mistakes, and it is the highest
  level available without redesigning goroutine ownership.
- **Why not higher yet.** Goroutine ownership is spread across 12,803 lines
  in `go/cmd/ftw` (4,169 of them in `main.go`). That redesign is step 5.

**2. Incomplete wiring (197).** A behaviour is added to one entry point and
missed on another. Examples: the setup wizard writes `cfg.ev_charger` but
never ships the Zaptec runtime driver (#977); OCPP chargers never receive
setpoints because only the plain send path was wrapped (#979); box-page
approvals do not return rejected results (#1646). Writes to Core arrive
through the HTTP API, the web UI, Home Assistant, the app protocol, the
CLI and the watchdog. Where a single door exists, it works.
`control.ApplyMode` is commented as "the one door into a mode change", and
the app protocol reuses the API handler through `Via(appproto.OpSetMode)`.
But callers still take `CtrlMu` themselves, and `internal/api` calls the
driver registry's `Send` directly in three places. Inferred from the
findings, not measured: most wiring misses are paths without such a door.
- **Level: architecture (step 5).** No lint can tell that a new entry point
  forgot a rule. Only a single door can guarantee it.

**3. Stored state on existing installs (102).** A change works on a fresh
box and breaks an upgraded one. Examples: a removed config key that old
YAML still sets (#124); legacy price rows left valid after a currency
marker was added (#748); a legacy YAML planner value skipped on upgrade
(#1018). Migrations live in `internal/state`
(ad hoc `ALTER TABLE`), `internal/config`, `internal/cars`,
`internal/driverrepo` and one-off functions in `cmd/ftw`. No test boots
Core on data from an older release.
- **Level: architecture plus a test (step 7).** One migration registry, and
  an upgrade corpus that CI boots.

**4. Missing or stale data used as a value (101).** Examples: offline EV
telemetry summed into the dispatch EV total (#36); stale PV readings used
as the live prediction anchor (#85); `siteLoadW` built from stale readings
and returning 0 when the meter is missing (#310). `telemetry.Store` keeps a
reading after its driver goes stale, and `Get`, `ReadingsByType` and
`ReadingsByDriver` return it unfiltered. Health filtering is opt-in. The
original grep found 147 matching lines outside the package, 35 of them `ReadingsByType`
calls, against 11 `TelemetryLive()` checks. Direct reads of `RawW` and
`SmoothedW` alone grew from 44 in 8 files (1 May) to 101 in 20 files
(17 October).
- **Level: lint now (step 2), then types (step 3).** The revised ratchet
  counts 150 occurrences on the proposal's base, including several on one
  line. It stops per-file count increases for four named read forms; it
  does not prove that an existing or replacement read uses live data.
  Step 3 aims to make the safe accessor the only one.

**5. Duplicated truth (95).** 40 of these findings are in `docs/`, where
prose restates code and drifts from it. Others are web constants that copy
Go, or Lua host helpers that docs promise but the host does not expose
(#20, #22).
- **Level: architecture (step 6).** Generate from one Go definition, or
  delete the prose.

## Backtest

For every finding, the backtest asked: had this structure existed before
the PR, would the mistake fail to compile, fail CI, or be impossible to
write? The judge (Codex) was told to be strict, so "might help" counts as
no. Treat the counts as hypotheses for setting priorities. They are not
an observed prevention rate, a causal estimate or a test of these draft
PRs. Each structure needs its own executable test and failure case.

| Proposed structure | Judged caught | P1 or High | Mostly in |
|---|---:|---:|---|
| S5 one source of truth, generated or deleted docs | 85 | 14 | duplicated-truth 54, incomplete-wiring 18 |
| S1 one write path into Core, shared case table per adapter | 69 | 28 | incomplete-wiring 36, concurrency 9 |
| S2 race detector and lifecycle harness in CI | 37 | 12 | concurrency 35 |
| S3 typed freshness at the telemetry boundary | 33 | 14 | unknown-as-value 32 |
| S4 migration registry plus upgrade corpus | 30 | 17 | legacy-state 27 |
| S6 distinct unit types for W, Wh, SoC, A, money | 8 | 1 | power-units-sign 7 |
| Any of the six | 257 of 1,150 | | |

The 257 total covers all six hypothetical structures. After dropping S6,
the union is 249 findings. Overlap means the rows must not be added. The
remaining 901 received no prevention label among S1–S5; that does not prove
that the labelled 249 would be prevented in practice.

The executable evidence has a narrower scope:

| Check | Evidence and limit |
|---|---|
| #1676 race detector | It fails on the parents of #787 and #789. Both races were in test fixtures, not production code. The new CI job passed at `bcc32d7d` in 4 min 20 s. This tests the detector on exercised paths; it does not prove absence of races, ordering bugs or shutdown bugs. |
| #1677 telemetry ratchet | Both the original line counter and the revised occurrence counter reject #36, #85, #310 and #647 against their parents. Rejection proves that a read count grew, not that the check identified the stale value itself. The revision adds tests for fields, multiple reads per line, exclusions, exceptions and Git errors on macOS and Linux. |
| #1678 simulator skill | The initial Linux run covered launch, doctor, idle and cleanup. Review then reproduced false success with missing feedback and with another instance answering the port. The revision adds process and port checks and requires fresh command evidence for both batteries. A macOS run on 17 October 2026 passed launch, doctor, idle and cleanup: both drivers accepted 0 W commands, all five ports closed, and the evidence remained. Helper tests cover missing feedback, stale commands, wrong listeners and reused PIDs on macOS and Linux. No hardware result follows from a simulator run. |

S2 includes a lifecycle harness as well as `-race`; #1676 adds only the race
detector. S3 assumes typed values that prevent unsafe number access;
#1677 adds only a count check. S1, S4 and S5 remain designs. None of their
model counts measures the protection shipped by these four drafts.

The earlier cost estimate (42 rejections over 1,423 first-parent commits,
33 from numbered PRs and 10 with an unknown-as-value label) used the old
line counter. It is not a measured cost for the revised occurrence check.

S6 remains out of scope. The model assigned it eight findings; the existing
`internal/units` reflection tests cover kWp and SoC scale. This is a scope
choice, not evidence that unit types have no further value.

## Steps

Each step merges on its own and leaves `master` working. They are ordered
by what the backtest and the proofs support, cheapest first.

1. **Race detector in CI.** Draft #1676. One job, one script, no product
   code. It catches data races on paths the tests execute. Changes to
   `race.sh` itself must select the Core CI job.
2. **Telemetry read ratchet.** Draft #1677. It rejects per-file increases
   in the four counted read forms, and its error names the safe accessor.
3. **Health-gated telemetry accessors** (S3, types). Add
   `Store.Live(driver, type) (Reading, bool)` and
   `Store.LiveByType(type) []Reading`. They return copies and skip stale,
   offline and faulted drivers. Migrate the counted reads package by package,
   with the ratchet proving each count only falls. Then make `Get` and
   `ReadingsByType` unexported. Returning `Reading` and `bool` alone does
   not provide S3's type guarantee: callers can ignore the flag or retain
   a reading past its freshness window. Prove the stronger boundary before
   removing the ratchet or claiming S3's predicted coverage.
4. **verify-ftw skill.** Draft #1678. It is a scripted launch, doctor,
   drive and cleanup over the simulators. The supported assertion is idle
   dispatch for both batteries. Other modes, driver fallback, UI and real
   hardware each need their own run and evidence.
5. **One write path into Core** (S1, architecture). Settings and control
   requests become typed commands handled by one Core service that owns the
   locks. The API, web, HA, app protocol, CLI and watchdog become adapters.
   One case table runs against every adapter. Start with the existing mode
   door (`control.ApplyMode`), then driver control and EV, then settings.
   An import check like `check-cloudlink-imports.sh` keeps adapters from
   reaching control internals. This step also splits `cmd/ftw/main.go`
   along the same seams. Run `/architect` with two competing sketches
   before writing code.
6. **One source of truth** (S5). Generate the web's shared constants and
   field lists from one definition, the way `appproto/gencontract` already
   turns `contract/registry.yaml` into Go constants for the box and FTW
   Cloud. Generate the Lua host API reference from
   `go/internal/drivers/lua.go`.
   Delete docs that restate code, and add a drift check to CI.
7. **Migration registry and upgrade corpus** (S4). Every change to
   persisted shapes registers one migration. CI boots Core on state
   captured from the last stable and the last beta, runs each migration
   twice, and compares the result.
8. **AGENTS.md rule table.** Shrink the prose and add a table that pairs
   each rule with its enforcer. This waits for #1364, which also edits
   AGENTS.md and has right of way.

## Proposed AGENTS.md rule table

Status of today's rules. "No check found" means I found nothing in
`.github/scripts`, `scripts/`, the Makefile or the workflows. Tests may
cover a rule indirectly.

| Rule | Enforced by |
|---|---|
| FTW Cloud uplink imports only `go/protocol` | `check-cloudlink-imports.sh` |
| `go/protocol` imports nothing from the box | `check-protocol-boundary.sh` |
| No agent planning documents | `check-no-planning-docs.sh` |
| User-visible change needs a changeset | `changeset-check.yml` |
| Database queries stay in `go/internal/state` | No check found. `internal/driverrepo` and `internal/backup` import `database/sql` today |
| Planner output is never sent directly to hardware | No check found. `internal/mpc` does not depend on `internal/drivers` today, so an import check would pass |
| Sign conversion only at the driver boundary | No check found |
| Stale site-meter data stops dispatch | Control tests; seen live in the verify-ftw run |
| No data race on exercised test paths | Step 1; logical lifecycle errors still need tests |
| No per-file increase in four counted telemetry read forms | Step 2; freshness itself needs step 3 |

The two "no check found" rules that pass today (planner and database)
cost one import check each, written the same way as the cloudlink check.
They are good first follow-ups once steps 1 and 2 land.

## What the prototypes establish

- *Does the race detector find anything today, or would the CI job be red
  on day one?* It passed on all 16 package trees at `3789764a` and in the
  draft's CI job. It fails on the two historical test-fixture races.
- *Can a check for stale reads be precise enough?* As a ratchet on new
  reads, the old counter rejected four historical PRs with stale-read
  findings. Negative tests exposed portability, same-line and exception
  gaps. The revised counter tests those cases; it still does not prove
  the health of a reading.
- *Can an agent run Core end to end without hardware?* Yes, with the
  simulators on isolated ports. The initial Linux run reported about 15
  seconds after the build. That run alone did not test false success or
  establish support on other operating systems.

## Left for the owner

- Whether the CI cost of the race job is worth it on every Core PR, or only
  on PRs that touch the listed packages.
- Whether `docs/agent-pitfalls/` stays in the repo, or the plan merges
  without it.
- The order of steps 5–7. The backtest ranks S5 and S1 above S4 by count,
  and S1 and S4 above S5 by P1/High findings.
