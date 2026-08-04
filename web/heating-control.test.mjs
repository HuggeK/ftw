import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

const source = readFileSync(new URL('./heating.js', import.meta.url), 'utf8');

function loadHeatingHarness(overrides = {}) {
  const section = { hidden: false };
  const grid = { innerHTML: '' };
  const context = {
    document: {
      readyState: 'loading',
      addEventListener() {},
      head: { appendChild() {} },
      createElement() { return {}; },
      getElementById(id) {
        if (id === 'heating-section') return section;
        if (id === 'heating-grid') return grid;
        return null;
      },
    },
    fetch: overrides.fetch || (() => Promise.resolve({ json: () => Promise.resolve({}) })),
    console,
  };
  const instrumented = source.replace(
    '  if (document.readyState === \'loading\') {',
    '  globalThis.__ftwHeatingTest = { controlBlock, refresh };\n\n  if (document.readyState === \'loading\') {'
  );
  assert.notEqual(instrumented, source, 'heating test hook anchor moved');
  vm.runInNewContext(instrumented, context);
  return { api: context.__ftwHeatingTest, section, grid };
}

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
  assert.match(source, /observedControlValue\(detail, control\)/);
  assert.match(source, /var value = held \? hold\.value : observed;/);
  assert.match(source, /controls wait for telemetry instead of assuming 0/);
});

test('an absolute step starts from reported offset telemetry', () => {
  assert.match(source, /hp_z1_heat_offset/);
  assert.match(source, /hp_heating_offset_climate_system_1/);
  assert.match(source, /clampControl\(value \+ delta, input\)/);
  assert.match(source, /!canAdjust \|\| disabled/);
});

test('the rendered absolute step uses the live offset and disables without it', () => {
  const { api } = loadHeatingHarness();
  const detail = {
    controls: [{ id: 'set_heat_curve_offset', label: 'Curve offset', evidence: 'readback', input: { type: 'number', min: -3, max: 3, step: 1, unit: '°C' } }],
    metrics: [{ name: 'hp_z1_heat_offset', value: 2 }],
  };
  const anchored = api.controlBlock('heat', detail);
  assert.match(anchored, /current \+2 °C/);
  assert.match(anchored, /data-hpc-value="3"/);

  const unknown = api.controlBlock('heat', { ...detail, metrics: [] });
  assert.match(unknown, /Current offset unavailable/);
  assert.match(unknown, /class="ftw-hpc-btn"[^>]*disabled/);
  assert.doesNotMatch(unknown, /data-hpc-value=/);
});

test('a refresh requested during an active cycle runs after it settles', async () => {
  let releaseFirst;
  let calls = 0;
  const detailCalls = [];
  const first = new Promise((resolve) => { releaseFirst = resolve; });
  const { api } = loadHeatingHarness({
    fetch(path) {
      calls += 1;
      if (calls === 1) return first;
      if (path === '/api/drivers/heat') {
        detailCalls.push(path);
        return Promise.resolve({ json: () => Promise.resolve({ metrics: [{ name: 'hp_power_w', value: 1 }] }) });
      }
      return Promise.resolve({ json: () => Promise.resolve({ points: [] }) });
    },
  });

  api.refresh();
  api.refresh();
  assert.equal(calls, 1, 'second request should queue while the first is active');
  releaseFirst({ json: () => Promise.resolve({ heat: {} }) });
  for (let i = 0; i < 20 && detailCalls.length < 2; i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  // The first cycle fetches the detail once for discovery and once for the
  // render; the queued cycle adds the third detail fetch.
  assert.equal(detailCalls.length, 3, 'queued request should run after the first cycle');
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
  // A refresh requested during a long cycle must run after that cycle settles.
  assert.match(source, /function refreshAfterControl\(\)/);
  assert.match(source, /if \(refreshInFlight\) \{ refreshQueued = true; return; \}/);
  assert.match(source, /if \(refreshQueued\) \{[\s\S]{0,80}refreshQueued = false;[\s\S]{0,80}refresh\(\);/);
});

test('stepper buttons rather than an input that a re-render would clear', () => {
  // The card is re-rendered wholesale every 30 s.
  assert.doesNotMatch(source, /class="ftw-hpc[^"]*"[^>]*<input/);
  assert.match(source, /data-hpc-value="/);
});
