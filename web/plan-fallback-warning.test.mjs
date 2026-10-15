import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

test("plan renders a visible optimizer fallback alert", () => {
  assert.match(plan, /id = 'plan-optimizer-fallback'/);
  assert.match(plan, /setAttribute\('role', 'alert'\)/);
  assert.match(plan, /Mathematical optimizer unavailable\. This plan uses the built-in Go fallback\./);
  assert.match(plan, /solver\.fallback_reason/);
});

// Core is the default planner, so "optimizer unavailable" must be gated on
// the fallback flag alone — a plan solved by the engine the operator chose is
// not a degradation, whichever engine that is.
test("the fallback alert is gated on the solver fallback flag", () => {
  assert.match(plan, /if \(!solver \|\| !solver\.fallback\) \{/);
  assert.doesNotMatch(plan, /engine === ['"]go-dp['"]/);
  assert.doesNotMatch(plan, /engine === ['"]core['"]/);
});

test("a reserve plan shows why its horizon ends early without inventing a missed departure", () => {
  const alert = { textContent: "" };
  const section = {};
  const document = { getElementById: id => id === "plan-section" ? section : alert };
  const start = plan.indexOf("  function renderOptimizerFallbackAlert(");
  const end = plan.indexOf("\n  function ", start + 5);
  const source = plan.slice(start, end) + "\nrenderOptimizerFallbackAlert(sample);";
  const reason = "Shorter plan: 12 of 24 intervals. Later forecasts cannot meet the site limits.";
  vm.runInNewContext(source, {
    document, evShortfallWh: () => 0,
    sample: { solver: { backend: "ev_reserve", fallback: true, fallback_reason: reason } },
  });
  assert.ok(alert.textContent.includes(reason));
  assert.doesNotMatch(alert.textContent, /short at departure/);
});
