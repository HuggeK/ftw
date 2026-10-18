// The car's reported levels already live in history.db as vehicle_soc.
// Keep these separate from the car runtime's energy-based level estimate.
const RANGES = ['24h', '7d', '30d'];

export function carLevelSources(car) {
  return [...new Set((Array.isArray(car?.sources) ? car.sources : []).filter(x => typeof x === 'string' && x))];
}

export function reportedLevelPoints(points = [], now = Date.now()) {
  return points.filter(p => p && Number.isFinite(p.ts) && p.ts > 0 && p.ts <= now &&
    Number.isFinite(p.v) && p.v >= 0 && p.v <= 1 && Number.isSafeInteger(p.n) && p.n > 0)
    .map(p => ({
      at: p.ts,
      first: Number.isFinite(p.first_ms) && p.first_ms > 0 ? p.first_ms : p.ts,
      resolution: Math.max(0, p.resolution_ms || 0),
      value: p.v * 100,
      min: Number.isFinite(p.min) && p.min >= 0 && p.min <= 1 ? p.min * 100 : p.v * 100,
      max: Number.isFinite(p.max) && p.max >= 0 && p.max <= 1 ? p.max * 100 : p.v * 100,
      last: Number.isFinite(p.last) && p.last >= 0 && p.last <= 1 ? p.last * 100 : p.n === 1 ? p.v * 100 : null,
      count: p.n,
    })).sort((a, b) => a.at - b.at);
}

async function readJSON(url, { fetcher, signal, timeoutMs = 30000 }) {
  signal?.throwIfAborted();
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal?.addEventListener('abort', abort, { once: true });
  const timer = setTimeout(abort, timeoutMs);
  try {
    const response = await fetcher(url, { signal: controller.signal, cache: 'no-store' });
    if (!response.ok) throw new Error('History read failed');
    return await response.json();
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener('abort', abort);
  }
}

// Empty sources can mean the viewer's car list hid them. Ask the box before
// saying that the car has none; an authorized empty list is different.
export async function readHistoryCar(car, { fetcher = fetch, signal } = {}) {
  if (carLevelSources(car).length) return { car, redacted: false };
  const data = await readJSON('/api/cars', { fetcher, signal });
  if (data.redacted) return { car, redacted: true };
  const found = data.cars?.find(c => c.id === car.id);
  if (!found) throw new Error('Car no longer available');
  return { car: { ...car, sources: carLevelSources(found) }, redacted: false };
}

export async function readCarLevelHistory(car, range, { fetcher = fetch, signal, now = Date.now(), timeoutMs = 30000 } = {}) {
  if (!RANGES.includes(range)) throw new Error('Unknown history range');
  return Promise.all(carLevelSources(car).map(async source => {
    try {
      const query = new URLSearchParams({ driver: source, metric: 'vehicle_soc', range, points: '180' });
      const data = await readJSON('/api/series?' + query, { fetcher, signal, timeoutMs });
      if (!Array.isArray(data.points)) throw new Error('History points missing');
      return { source, points: reportedLevelPoints(data.points, now), failed: false };
    } catch (err) {
      if (signal?.aborted) throw err;
      return { source, points: [], failed: true };
    }
  }));
}

function node(doc, tag, parent, text, className) {
  const el = doc.createElement(tag);
  if (text != null) el.textContent = text;
  if (className) el.className = className;
  if (parent) parent.appendChild(el);
  return el;
}

function clock(ms) {
  return new Date(ms).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

export function pointDescription(p) {
  if (p.count === 1) return `${p.last.toFixed(1)}% · ${clock(p.first)}`;
  return `${p.value.toFixed(1)}% average · ${p.min.toFixed(1)}–${p.max.toFixed(1)}% · ${p.count} reports · ${clock(p.at)}`;
}

function plot(doc, host, points, since, until) {
  const ns = 'http://www.w3.org/2000/svg';
  const svg = doc.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 640 210');
  svg.setAttribute('role', 'group');
  svg.setAttribute('aria-label', 'Reported car battery levels, from 0 to 100 percent');
  const shape = (tag, attrs, text) => {
    const el = doc.createElementNS(ns, tag);
    for (const [name, value] of Object.entries(attrs)) el.setAttribute(name, String(value));
    if (text != null) el.textContent = text;
    svg.appendChild(el);
    return el;
  };
  const x = ms => 42 + Math.max(0, Math.min(1, (ms - since) / (until - since))) * 580;
  const y = pct => 174 - pct * 1.5;
  for (const pct of [0, 50, 100]) {
    shape('line', { x1: 42, x2: 622, y1: y(pct), y2: y(pct), class: 'soc-grid' });
    shape('text', { x: 32, y: y(pct) + 4, 'text-anchor': 'end', class: 'soc-axis' }, `${pct}%`);
  }
  const selected = node(doc, 'p', null, 'Select a point to read its saved level and time.', 'soc-note');
  selected.setAttribute('role', 'status');
  // Dots are recorded reports. No line fills the time between sparse reports.
  for (const p of points) {
    if (p.min !== p.max) shape('line', { x1: x(p.at), x2: x(p.at), y1: y(p.min), y2: y(p.max), class: 'soc-envelope' });
    const dot = shape('circle', { cx: x(p.count === 1 ? p.first : p.at), cy: y(p.value), r: 3.5, class: 'soc-dot', tabindex: 0 });
    const desc = pointDescription(p);
    dot.setAttribute('aria-label', desc);
    dot.setAttribute('role', 'button');
    for (const event of ['pointerenter', 'focus', 'click']) dot.addEventListener(event, () => { selected.textContent = desc; });
    dot.addEventListener('keydown', event => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault(); selected.textContent = desc;
    });
    const title = doc.createElementNS(ns, 'title'); title.textContent = desc; dot.appendChild(title);
  }
  shape('text', { x: 42, y: 200, class: 'soc-axis' }, clock(since));
  shape('text', { x: 622, y: 200, 'text-anchor': 'end', class: 'soc-axis' }, clock(until));
  svg.addEventListener('pointerdown', event => {
    const bounds = svg.getBoundingClientRect();
    if (!bounds.width || !points.length) return;
    const atX = (event.clientX - bounds.left) / bounds.width * 640;
    const nearest = points.reduce((best, p) => Math.abs(x(p.at) - atX) < Math.abs(x(best.at) - atX) ? p : best);
    selected.textContent = pointDescription(nearest);
  });
  host.appendChild(svg);
  host.appendChild(selected);
}

function styles(doc) {
  if (doc.getElementById('car-soc-history-styles')) return;
  const el = node(doc, 'style', doc.head);
  el.id = 'car-soc-history-styles';
  el.textContent = `
    .car-soc-history{box-sizing:border-box;width:min(720px,94vw);max-height:90dvh;margin:auto;padding:0;border:1px solid var(--line);border-radius:12px;background:var(--ink-raised);color:var(--fg);overflow:auto}
    .car-soc-history::backdrop{background:rgb(0 0 0 / .65)}
    .car-soc-history header{display:flex;align-items:center;justify-content:space-between;gap:16px;padding:18px;border-bottom:1px solid var(--line)}
    .car-soc-history h2{font-size:1.1rem;margin:0}.car-soc-history h3{font-size:.95rem;margin:0}
    .car-soc-history button{font:inherit;color:var(--fg);border:1px solid var(--line);border-radius:8px;padding:7px 12px;background:transparent;cursor:pointer}
    .car-soc-history button[aria-pressed=true]{background:var(--accent-e);color:#0a0a0a}
    .car-soc-history button:focus-visible{outline:2px solid var(--accent-e);outline-offset:3px}
    .car-soc-history .soc-body{padding:18px}.car-soc-history .soc-ranges{display:flex;gap:8px;margin-bottom:14px}
    .car-soc-history .soc-note{font-size:.82rem;color:var(--fg-muted);line-height:1.5;margin:10px 0}
    .car-soc-history .soc-source{border-top:1px solid var(--line);padding-top:14px;margin-top:14px}
    .car-soc-history .soc-last{font:1.3rem var(--mono);margin:8px 0}
    .car-soc-history svg{display:block;width:100%;height:auto;min-height:130px;overflow:visible}
    .car-soc-history .soc-grid{stroke:var(--line)}.car-soc-history .soc-axis{font:11px var(--mono);fill:var(--fg-muted)}
    .car-soc-history .soc-dot{fill:var(--cyan)}.car-soc-history .soc-dot:focus{stroke:var(--fg);stroke-width:2}
    .car-soc-history .soc-envelope{stroke:var(--cyan);stroke-width:2}
    @media(max-width:480px){.car-soc-history .soc-axis{font-size:24px}}
  `;
}

export function openCarLevelHistory(doc, car, { fetcher = fetch,
  requestLogin = () => doc.defaultView?.FTWLanAuth?.ensure?.() || Promise.resolve(false) } = {}) {
  styles(doc);
  const dialog = node(doc, 'dialog', doc.body, null, 'car-soc-history');
  const head = node(doc, 'header', dialog);
  const title = node(doc, 'h2', head, `${car.name || 'Car'} · battery history`);
  title.id = 'car-level-history-title';
  dialog.setAttribute('aria-labelledby', title.id);
  const close = node(doc, 'button', head, 'Close'); close.type = 'button';
  close.addEventListener('click', () => dialog.close());
  const body = node(doc, 'div', dialog, null, 'soc-body');
  const ranges = node(doc, 'div', body, null, 'soc-ranges');
  ranges.setAttribute('role', 'group'); ranges.setAttribute('aria-label', 'History range');
  node(doc, 'p', body, 'Reported sensor levels saved by the box. Charging estimates and levels set by hand are not plotted.', 'soc-note');
  const content = node(doc, 'div', body); content.setAttribute('aria-live', 'polite');
  let range = '24h', controller = null, sequence = 0, closed = false;
  const buttons = RANGES.map(value => {
    const button = node(doc, 'button', ranges, value === '24h' ? '24 hours' : value === '7d' ? '7 days' : '30 days');
    button.type = 'button'; button.addEventListener('click', () => { range = value; load(); });
    return [value, button];
  });
  async function load() {
    const current = ++sequence;
    controller?.abort(); controller = new AbortController();
    const signal = controller.signal;
    buttons.forEach(([value, button]) => button.setAttribute('aria-pressed', String(value === range)));
    content.replaceChildren(); node(doc, 'p', content, 'Loading saved levels…', 'soc-note');
    try {
      const resolved = await readHistoryCar(car, { fetcher, signal });
      if (closed || current !== sequence) return;
      if (resolved.redacted) {
        content.replaceChildren();
        node(doc, 'p', content, 'Sign in to read this car’s sensor history.', 'soc-note');
        const login = node(doc, 'button', content, 'Sign in'); login.type = 'button';
        login.addEventListener('click', async () => {
          // The house-password form must be above the page, not behind a
          // native modal. Reopen history after it has finished.
          dialog.close();
          if (await requestLogin()) openCarLevelHistory(doc, car, { fetcher, requestLogin });
        });
        return;
      }
      car = resolved.car;
      const until = Date.now();
      const since = until - (range === '24h' ? 1 : range === '7d' ? 7 : 30) * 86400000;
      const sources = await readCarLevelHistory(car, range, { fetcher, signal, now: until });
      if (closed || current !== sequence) return;
      content.replaceChildren();
      if (!sources.length) node(doc, 'p', content, 'No sensor source is available for this car on this device.', 'soc-note');
      for (const [index, source] of sources.entries()) {
        const section = node(doc, 'section', content, null, 'soc-source');
        node(doc, 'h3', section, sources.length === 1 ? 'Sensor reports' : `Sensor ${index + 1}`);
        if (source.failed) {
          node(doc, 'p', section, 'The box could not read this history. Try the range again.', 'soc-note');
        } else if (!source.points.length) {
          node(doc, 'p', section, 'No saved sensor reports in this period.', 'soc-note');
        } else {
          const last = source.points.at(-1);
          node(doc, 'p', section, last.last == null
            ? `${last.value.toFixed(1)}% · latest interval average`
            : `${last.last.toFixed(1)}% · last saved level`, 'soc-last');
          node(doc, 'p', section, pointDescription(last), 'soc-note');
          plot(doc, section, source.points, since, until);
        }
      }
      node(doc, 'p', content, 'Each dot is a saved report or the average of reports in one interval. Vertical bars show the reported range. Time between reports is not filled.', 'soc-note');
    } catch (err) {
      if (closed || current !== sequence) return;
      content.replaceChildren();
      node(doc, 'p', content, 'The box did not finish reading history. Try the range again.', 'soc-note');
    }
  }
  dialog.addEventListener('close', () => {
    closed = true; controller?.abort(); dialog.remove();
    [...doc.querySelectorAll('[data-history-car]')].find(button => button.dataset.historyCar === car.id)?.focus();
  });
  dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); });
  dialog.showModal(); load();
  return dialog;
}
