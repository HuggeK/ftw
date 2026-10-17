"""Agent-pitfall lever for srcfl/ftw.

  python3 findings.py extract  review_comments.json findings.json
  python3 findings.py classify findings.json labels.json [model]
  python3 findings.py backtest findings.json backtest.json [model]
  python3 findings.py report   findings.json labels.json backtest.json

Findings are inline comments from review bots (Codex, Cursor Bugbot, Copilot)
on PRs that agents wrote. Classification and backtest are LLM judgments made
in batches of 40 through an agent CLI (grok, codex or claude); outputs are resumable JSON files.
"""
import json, re, subprocess, sys, os, concurrent.futures as cf
from collections import Counter, defaultdict

BOTS = {"chatgpt-codex-connector[bot]", "cursor[bot]", "Copilot"}
TAX = """legacy-state: change ignores state already stored on existing installs (DB rows, saved settings, YAML seeds, migrations, upgrade/rollback paths, retired keys).
unknown-as-value: missing, stale, unknown or failed data is treated as a real value (0, default, last value, success); freshness/health ignored.
concurrency-lifecycle: races, ordering, locks, cancellation, timeouts, restart/shutdown, retries, non-idempotent steps.
power-units-sign: sign convention, units (W/kW/Wh), power vs energy, phases, thresholds, clamps, money/price math.
duplicated-truth: the same constant, list, rule or logic lives in several places and diverges; docs/comments/tests contradict code.
incomplete-wiring: new behaviour not applied on every entry point/caller/path, or a refactor drops a caller/path that still mattered.
silent-failure: errors swallowed, fallback hides a failure, fail-open, wrong status reported as success.
security: auth, secrets, tokens, injection, exposure, permissions.
release-ci: release workflows, CI scripts, changeset/version bump, build/packaging.
ui-contract: UI shows state or controls that do not match backend data/state.
other: none of the above."""
CLASSES = [l.split(":")[0] for l in TAX.splitlines()]
S = {
 "S1": "One write path: every settings change and control request goes through one typed Core service (core.Apply(cmd)); API, web UI, HA/MQTT, app protocol, CLI and watchdog are thin adapters, and one shared case table runs against every adapter.",
 "S2": "Race detector and lifecycle harness in CI: go test -race on core packages in CI, goroutines owned by a Run(ctx)/Stop lifecycle helper with tests for cancel, restart, shutdown and retry.",
 "S3": "Typed freshness at the telemetry boundary: readings are Observed values with state Live/Stale/Missing/Failed; code can only get a number via Live(now), so missing or stale data cannot be read as 0 or as the last value; Lua host rejects nil-to-0.",
 "S4": "Migration registry plus upgrade corpus: every change to persisted shapes (config document, YAML seed, SQLite schema, saved device/session state) needs an entry in one registry; CI boots Core on fixtures from older releases and runs each migration twice.",
 "S5": "One source of truth: shared constants, enums, field lists and API shapes are generated from one Go definition for web, docs and drivers; docs that restate code are generated or deleted; a check fails on drift.",
 "S6": "Unit types: power, energy, SoC, current and money are distinct Go types (units.W, units.Wh, units.SoC...) in control/mpc/api signatures, with sign convention encoded in the type; raw float64 for these is rejected by lint.",
}

def clean(b):
    b = re.sub(r"!\[[^\]]*\]\([^)]*\)|<[^>]+>|Useful\? React with.*", " ", b, flags=re.S)
    return re.sub(r"\s+", " ", b).strip()

def prio(b):
    m = re.search(r"\bP([0-3])\b", b)
    if m: return "P" + m.group(1)
    m = re.search(r"(High|Medium|Low) Severity", b)
    return m.group(1) if m else "none"

def extract(src, dst):
    out = []
    for c in json.load(open(src)):
        if c["user"]["login"] not in BOTS: continue
        t = clean(c["body"])
        out.append({"url": c["html_url"], "pr": int(c["pull_request_url"].rsplit("/", 1)[1]), "path": c["path"],
                    "bot": c["user"]["login"], "at": c["created_at"], "priority": prio(c["body"]),
                    "title": re.sub(r"\*+", "", t)[:140].strip(), "text": t[:450]})
    json.dump(out, open(dst, "w"), indent=1); print(len(out), "findings")

RUNNERS = {
    "grok": lambda p: ["grok", "-p", p, "--max-turns", "1", "--no-subagents", "--disable-web-search", "--tools", "", "--reasoning-effort", "low"],
    "codex": lambda p: ["codex", "exec", "--skip-git-repo-check", p],
    "sonnet": lambda p: ["claude", "-p", "--model", "sonnet", p],
}

def ask(prompt, model):
    r = subprocess.run(RUNNERS[model](prompt), stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=900, cwd="/tmp")
    res = {}
    for l in r.stdout.splitlines():
        l = l.strip().strip(",")
        if l.startswith("{"):
            try:
                j = json.loads(l); res[str(j["id"])] = j
            except Exception: pass
    return res

def batched(findings, out, make_prompt, parse, model):
    done = json.load(open(out)) if os.path.exists(out) else {}
    todo = [(str(i), o) for i, o in enumerate(findings) if str(i) not in done]
    chunks = [todo[i:i + 40] for i in range(0, len(todo), 40)]
    with cf.ThreadPoolExecutor(5) as ex:
        for res in ex.map(lambda ch: ask(make_prompt(ch), model), chunks):
            for k, j in res.items(): done[k] = parse(j)
            json.dump(done, open(out, "w")); print(len(done), flush=True)

def lines(ch):
    return "\n".join(json.dumps({"id": int(i), "file": o["path"], "finding": o["text"]}, ensure_ascii=False) for i, o in ch)

def classify(src, out, model="grok"):
    p = lambda ch: ("You classify code-review findings raised by review bots on AI-agent-written PRs in a Go/Lua/JS home energy system.\n"
                    "Pick exactly one class: the root mistake the reviewer caught in the agent's change.\n" + TAX +
                    "\n\nReturn ONLY JSON lines, one per input, like {\"id\": 3, \"c\": \"legacy-state\"}. No prose.\n\n" + lines(ch))
    batched(json.load(open(src)), out, p, lambda j: j["c"] if j.get("c") in CLASSES else "other", model)

def backtest(src, out, model="grok"):
    sl = "\n".join(f"{k}: {v}" for k, v in S.items())
    p = lambda ch: ("These are review findings that bots raised on AI-agent-written PRs in a Go/Lua/JS home energy system (FTW).\n"
                    "Candidate structural fixes:\n" + sl + "\n\nFor each finding, list the structures that would have CAUGHT it: had the structure existed, "
                    "the mistake would not compile, would fail CI, or could not be written. Be strict: 'might help' is not caught. An empty list is common.\n"
                    "Return ONLY JSON lines like {\"id\": 3, \"s\": [\"S3\"]}.\n\n" + lines(ch))
    batched(json.load(open(src)), out, p, lambda j: [s for s in j.get("s", []) if s in S], model)

def report(src, lab, bt):
    f = json.load(open(src)); L = json.load(open(lab)); B = json.load(open(bt)) if os.path.exists(bt) else {}
    cls = Counter(); prs = defaultdict(set); hi = Counter()
    for i, o in enumerate(f):
        c = L.get(str(i), "unlabeled"); cls[c] += 1; prs[c].add(o["pr"]); hi[c] += o["priority"] in ("P1", "High")
    print(f"findings={len(f)} prs={len({o['pr'] for o in f})} labeled={len(L)} backtested={len(B)}")
    for c, n in cls.most_common(): print(f"  {c:22} {n:4}  PRs={len(prs[c]):3}  P1/High={hi[c]}")
    if B:
        caught = Counter(); caught_hi = Counter(); by = defaultdict(Counter); anyc = 0
        for i, o in enumerate(f):
            ss = B.get(str(i), []); anyc += bool(ss)
            for s in ss:
                caught[s] += 1; caught_hi[s] += o["priority"] in ("P1", "High"); by[s][L.get(str(i), "?")] += 1
        print(f"backtest: caught by any structure {anyc}/{len(B)}")
        for s, n in caught.most_common(): print(f"  {s} {n:4} P1/High={caught_hi[s]:3}  top classes {by[s].most_common(3)}")

if __name__ == "__main__":
    cmd, *a = sys.argv[1:]
    {"extract": extract, "classify": classify, "backtest": backtest, "report": report}[cmd](*a)
