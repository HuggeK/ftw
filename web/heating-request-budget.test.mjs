import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const source = readFileSync(new URL('./heating.js', import.meta.url), 'utf8');

test('heat-pump history is cached instead of range-scanned every live refresh', () => {
  assert.match(source, /HISTORY_REFRESH_MS\s*=\s*300000/);
  assert.match(source, /historyCache\[name\]\s*=\s*\{\s*at:/);
  assert.match(source, /fetchPumpHistory\(n\)/);
});

test('all-signals view does not fetch one history series per NIBE point', () => {
  assert.match(source, /metrics\.filter\(function \(m\) \{ return !!infoForKey\(m\.name\); \}\)\.forEach/);
});

test('overlapping dashboard refreshes queue one follow-up cycle', () => {
  assert.match(source, /if \(refreshInFlight\) \{ refreshQueued = true; return; \}/);
  assert.match(source, /var refreshQueued = false;/);
  assert.match(source, /function finishRefresh\(\)/);
  assert.match(source, /refreshInFlight = false;/);
});
