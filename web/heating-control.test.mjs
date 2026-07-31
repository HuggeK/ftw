import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const source = readFileSync(new URL('./heating.js', import.meta.url), 'utf8');

// These guard the invariants that break silently. The behaviour itself was
// verified against a running FTW and a probe driver: pressing + drove the
// driver's own hp_z1_heat_offset metric to 1, raise disabled at the declared
// +3, Release returned the metric to 0 via driver_default_mode, and a click
// on the control did not open the all-signals detail while a click on the
// card still did.

test('the control is rendered from the driver declaration, never from a driver name', () => {
  assert.match(source, /function controlBlock\(name, detail\)/);
  assert.match(source, /firstNumberControl\(detail\)/);
  // No control branch may key on a driver id — that is the mistake Settings
  // made, and the reason a declared control exists at all. Scoped to the
  // control code: the file's telemetry half names drivers freely and should.
  const start = source.indexOf('// ---- Control: one declared command per pump ----');
  const end = source.indexOf('// ---- Detail drill-in');
  assert.ok(start > 0 && end > start, 'control section markers moved');
  assert.doesNotMatch(source.slice(start, end), /heishamon|myuplink|nibe_local/i);
});

test('a pump that declares nothing renders nothing', () => {
  assert.match(source, /if \(!control\) return '';/);
});

test('bounds and step come from the declaration, not from constants here', () => {
  assert.match(source, /input\.step === 'number' && input\.step > 0 \? input\.step : 1/);
  assert.match(source, /typeof input\.min === 'number' && value <= input\.min/);
  assert.match(source, /typeof input\.max === 'number' && value >= input\.max/);
});

test('commanding the pump does not navigate into its signals', () => {
  // The whole card is a button; without stopPropagation the detail view opens
  // over the control the operator just pressed.
  assert.match(source, /closest\('\.ftw-hpc-btn'\)[\s\S]{0,120}e\.stopPropagation\(\)/);
  assert.match(source, /closest\('\.ftw-hpc-release'\)[\s\S]{0,120}e\.stopPropagation\(\)/);
});

test('no hold reads as Auto rather than a number we do not know', () => {
  assert.match(source, /ftw-hpc-auto">Auto</);
});

test('held state is carried by text and weight, not by colour alone', () => {
  // The theme's green/red pair is not separable under deuteranopia, so the
  // control must not encode its state in colour.
  const styles = source.slice(source.indexOf('.ftw-hpc{'), source.indexOf('.ftw-hpc-err{'));
  assert.doesNotMatch(styles, /var\(--green\)|var\(--red\)|var\(--accent\)/);
  assert.match(source, /\.ftw-hpc-value\{[^}]*font-weight:600/);
});

test('a driver that cannot confirm its writes says so in the UI', () => {
  assert.match(source, /control\.evidence === 'readback'/);
  assert.match(source, /does not confirm this setting/);
});

test('the operator sees the result of a press even mid-refresh', () => {
  // refresh() drops overlapping calls, which is right for the 30 s timer and
  // wrong immediately after a button press.
  assert.match(source, /function refreshAfterControl\(\)/);
  assert.match(source, /if \(!refreshInFlight\) \{ refresh\(\); return; \}/);
});

test('stepper buttons rather than an input that a re-render would clear', () => {
  // The card is re-rendered wholesale every 30 s.
  assert.doesNotMatch(source, /class="ftw-hpc[^"]*"[^>]*<input/);
  assert.match(source, /data-hpc-value="/);
});
