import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

const badge = readFileSync(new URL("./update-badge.js", import.meta.url), "utf8");
const theme = readFileSync(new URL("./components/theme.css", import.meta.url), "utf8");

// The corner badge carries two unrelated meanings: a filled dot means "an
// update is waiting, open me", and the warning means the planner has fallen
// back to the Go solver — a state that often needs no action at all. Both
// rendered as the same pulsing amber until #690, and a field tester read the
// warning as an update icon. These tests pin the cues that keep them apart.

function warningRule() {
  const start = badge.indexOf(".badge.warning {");
  assert.notEqual(start, -1, ".badge.warning rule not found in _styles()");
  return badge.slice(start, badge.indexOf("}", start));
}

describe("update badge warning state", () => {
  it("marks the optimizer warning with its own class and glyph", () => {
    assert.match(badge, /showOptimizerWarning \? " warning" : ""/);
    assert.match(badge, /showOptimizerWarning \? "!" : "●"/);
    assert.match(badge, /aria-label="\$\{showOptimizerWarning \? "Planner fallback active" : "Update available"\}"/);
  });

  it("stops pulsing, because the state is not an affordance", () => {
    assert.match(warningRule(), /animation:\s*none/);
    // The update dot keeps its pulse — that is the cue being contrasted.
    const dotRule = badge.slice(badge.indexOf(".badge {"), badge.indexOf(".badge.warning {"));
    assert.match(dotRule, /animation:\s*pulse/);
  });

  it("draws a ring rather than a filled dot, so shape carries the meaning", () => {
    const rule = warningRule();
    assert.match(rule, /border:\s*[\d.]+px solid currentColor/);
    assert.match(rule, /border-radius:\s*50%/);
    // Without an explicit box the ring collapses onto the glyph's own metrics.
    assert.match(rule, /width:\s*[\d.]+rem/);
    assert.match(rule, /height:\s*[\d.]+rem/);
    assert.match(rule, /box-sizing:\s*border-box/);
  });

  it("reuses the palette's dimmer amber instead of introducing a hue", () => {
    const rule = warningRule();
    assert.match(rule, /color:\s*var\(--amber-d,\s*#c08000\)/);
    assert.doesNotMatch(rule, /--accent-e/);
    // Amber stays the single accent and green stays the connection dot.
    assert.doesNotMatch(rule, /--(green|red|cyan|violet)/);
  });

  it("keeps --amber-d themed in both light and dark", () => {
    const dark = theme.slice(theme.indexOf(":root {"), theme.indexOf('html[data-theme="light"]'));
    const light = theme.slice(theme.indexOf('html[data-theme="light"]'));
    assert.match(dark, /--amber-d:\s*oklch\(/);
    assert.match(light, /--amber-d:\s*oklch\(/);
  });
});
