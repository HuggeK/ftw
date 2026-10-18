import assert from 'node:assert/strict';
import { test } from 'node:test';
import { carLevelSources, pointDescription, readCarLevelHistory, readHistoryCar, reportedLevelPoints } from './car-soc-history.js';

const now = 1792330436520;
const point = { ts: now - 10000, first_ms: now - 9021, v: 0.57, last: 0.57, min: 0.57, max: 0.57, n: 1, resolution_ms: 10000 };

test('only linked sensor sources are used, once each', () => {
  assert.deepEqual(carLevelSources({ sources: ['sensor-one', null, '', 'sensor-two', 'sensor-one'] }), ['sensor-one', 'sensor-two']);
  for (const car of [null, {}, { sources: 'not-a-list' }]) assert.deepEqual(carLevelSources(car), []);
});

test('stored fractional SoC, including zero, is displayed as percent with its source time', () => {
  const points = reportedLevelPoints([point, { ...point, ts: now - 20000, v: 0, last: 0, min: 0, max: 0 }], now);
  assert.equal(points[0].value, 0);
  assert.ok(Math.abs(points[1].value - 57) < 1e-10);
  assert.equal(points[1].first, point.first_ms);
  assert.equal(points[1].at, point.ts);
  assert.match(pointDescription(points[1]), /^57\.0% · /);
});

test('missing, nonfinite, future and invalid reports never become zero or 100 percent', () => {
  const bad = [null, { ...point, v: null }, { ...point, v: NaN }, { ...point, v: Infinity },
    { ...point, v: -0.1 }, { ...point, v: 57 }, { ...point, v: 1.1 },
    { ...point, ts: now + 1 }, { ...point, n: 0 }, { ...point, n: '1' }];
  assert.deepEqual(reportedLevelPoints(bad, now), []);
});

test('an aggregate retains its average, last reported value, envelope and count', () => {
  const [p] = reportedLevelPoints([{ ...point, v: 0.55, last: 0.57, min: 0.53, max: 0.57, n: 3 }], now);
  for (const [value, expected] of [[p.value, 55], [p.last, 57], [p.min, 53], [p.max, 57]]) assert.ok(Math.abs(value - expected) < 1e-10);
  assert.equal(p.count, 3);
  assert.match(pointDescription(p), /average · 53\.0–57\.0% · 3 reports/);
});

test('an aggregate without a last value never calls its average the last reported level', () => {
  const [p] = reportedLevelPoints([{ ...point, n: 63, v: 0.548, last: undefined }], now);
  assert.equal(p.last, null);
  assert.match(pointDescription(p), /^54\.8% average/);
});

test('history reads use the linked source and vehicle_soc, with bounded points and no estimates', async () => {
  const calls = [];
  const car = { sources: ['sensor/&?'], soc_permille: 990, soc_estimate: true, current_soc: 0.99 };
  const result = await readCarLevelHistory(car, '7d', { now, fetcher: async (url, opts) => {
    calls.push([url, opts]); return { ok: true, json: async () => ({ points: [point] }) };
  } });
  const url = new URL(calls[0][0], 'http://localhost');
  assert.equal(url.searchParams.get('driver'), 'sensor/&?');
  assert.equal(url.searchParams.get('metric'), 'vehicle_soc');
  assert.equal(url.searchParams.get('range'), '7d');
  assert.equal(url.searchParams.get('points'), '180');
  assert.equal(calls[0][1].cache, 'no-store');
  assert.ok(Math.abs(result[0].points[0].last - 57) < 1e-10);
});

test('a failed read and an empty period remain distinct, without discarding another source', async () => {
  const result = await readCarLevelHistory({ sources: ['good', 'empty', 'failed'] }, '24h', { now, fetcher: async url => {
    const source = new URL(url, 'http://localhost').searchParams.get('driver');
    return { ok: source !== 'failed', json: async () => ({ points: source === 'good' ? [point] : [] }) };
  } });
  assert.deepEqual(result.map(x => [x.source, x.failed, x.points.length]), [['good', false, 1], ['empty', false, 0], ['failed', true, 0]]);
});

test('a malformed body is a read failure, not an empty history', async () => {
  const [result] = await readCarLevelHistory({ sources: ['sensor'] }, '24h', { fetcher: async () => ({ ok: true, json: async () => ({}) }) });
  assert.equal(result.failed, true);
});

test('closing or changing the range cancels in-flight reads', async () => {
  const controller = new AbortController();
  const reading = readCarLevelHistory({ sources: ['sensor'] }, '24h', { signal: controller.signal, fetcher: (url, opts) => new Promise((resolve, reject) => {
    opts.signal.addEventListener('abort', () => reject(new DOMException('Cancelled', 'AbortError')));
  }) });
  controller.abort();
  await assert.rejects(reading, { name: 'AbortError' });
});

test('no linked source makes no history request and an unknown range is rejected', async () => {
  assert.deepEqual(await readCarLevelHistory({ sources: [] }, '24h', { fetcher: () => { throw new Error('unexpected request'); } }), []);
  await assert.rejects(readCarLevelHistory({ sources: ['sensor'] }, 'forever'), /Unknown history range/);
});

test('a redacted source list asks for sign-in instead of reporting no sensor', async () => {
  const car = { id: 'car-one' };
  const result = await readHistoryCar(car, { fetcher: async url => {
    assert.equal(url, '/api/cars');
    return { ok: true, json: async () => ({ cars: [car], redacted: true }) };
  } });
  assert.equal(result.redacted, true);
});

test('after sign-in the protected list supplies sources; a known empty list stays empty', async () => {
  for (const sources of [[], ['sensor']]) {
    const result = await readHistoryCar({ id: 'car-one' }, { fetcher: async () => ({ ok: true,
      json: async () => ({ cars: [{ id: 'car-one', sources }] }) }) });
    assert.equal(result.redacted, false);
    assert.deepEqual(result.car.sources, sources);
  }
});

test('a source deadline keeps another source’s completed history', async () => {
  const result = await readCarLevelHistory({ sources: ['good', 'slow'] }, '24h', { now, timeoutMs: 10,
    fetcher: async (url, { signal }) => {
      if (url.includes('driver=good')) return { ok: true, json: async () => ({ points: [point] }) };
      return new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new DOMException('Timed out', 'AbortError'))));
    } });
  assert.equal(result[0].failed, false);
  assert.equal(result[0].points.length, 1);
  assert.equal(result[1].failed, true);
});
