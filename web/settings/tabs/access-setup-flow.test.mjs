import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';

const source = readFileSync(new URL('./access.js', import.meta.url), 'utf8');
const setup = 'ftw.setup.write';

function rig(renew = false, cloud = {link:'on',cloudTermsAccepted:true,statisticsOnly:false}) {
  function element(tag) {
    const e = { tag, children: [], listeners: {}, checked: false, value: '',
      setAttribute(k, v) { this[k] = v; },
      addEventListener(k, v) { this.listeners[k] = v; },
      appendChild(child) { this.children.push(child); if (tag === 'select' && (!this.value || child.selected)) this.value = child.value; return child; },
      all() { return this.children.flatMap(c => [c, ...c.all()]); },
      querySelectorAll(q) { return this.all().filter(n => {
        if (q.startsWith('[id^=')) return n.id?.startsWith(q.slice(6, -2));
        const m = /^(input|select)\[name="(\w+)"\]$/.exec(q);
        return m && n.tag === m[1] && n.name === m[2];
      }); },
      querySelector(q) { return this.querySelectorAll(q)[0] || null; },
      submit() { this.listeners.submit({ preventDefault() {} }); },
      click() { this.listeners.click?.(); },
    };
    let text = '';
    Object.defineProperty(e, 'textContent', { get() { return text; }, set(v) { text = v; this.children = []; } });
    return e;
  }
  const roots = Object.fromEntries(['access-ready', 'access-code', 'access-code-form', 'access-lookup', 'access-note', 'access-offer', 'access-rows', 'access-rows-section', 'access-requests', 'access-requests-section'].map(id => [id, Object.assign(element('div'), { id })]));
  const all = () => Object.values(roots).flatMap(e => [e, ...e.all()]);
  const posts = [];
  const row = { id: 'agent1', role: 'agent', scopes: ['ftw.config.read', setup], ops: [], caps: {}, epoch: 1, expiresAtMs: Date.now() + 86400000, principal: { client: 'ChatGPT' } };
  const registry = { agentOps: {}, writeScopes: [setup], callScopes: [setup] };
  const answer = body => Promise.resolve({ ok: true, status: 200, json: async () => body });
  const context = vm.createContext({ window: {}, document: {
    createElement: element, getElementById: id => all().find(e => e.id === id) || null,
  }, fetch: (path, opts) => {
    if (opts?.method === 'POST') posts.push({ path, body: JSON.parse(opts.body) });
    if (path === '/api/cloud') return answer(cloud);
      if (path === '/api/auth/status') return answer({ lan_auth: true, configured: true });
    if (path === '/api/agents' && !opts) return answer({ agents: renew ? [row] : [], requests: [], registry, box: { widen: true, offers: true, clock: 'ok', nowMs: Date.now(), lookup: '/lookup', longestMs: { agent: 31536000000 } } });
    if (path.startsWith('/lookup?')) return answer({ offer: 'closed-fixture', status: 'pending' });
    if (path === '/api/agents/offer') return answer({ ...row, code: 'TESTCODE', client: 'ChatGPT', ttlS: 2592000, expMs: Date.now() + 600000 });
    return answer(row);
  }, setTimeout() {}, confirm: () => true, console });
  vm.runInContext(source, context);
  context.window.FTWSettings.tabs.access.after({ navigateTab() {} });
  return { roots, posts, all };
}
const settle = async () => { for (let n = 0; n < 12; n++) await Promise.resolve(); };

for (const allow of [false, true]) test(`new offer sends setup only when selected (${allow})`, async () => {
  const r = rig(); await settle();
  r.roots['access-code'].value = 'TESTCODE';
  r.roots['access-code-form'].submit(); await settle();
  const form = r.roots['access-offer'].all().find(e => e.tag === 'form');
  assert.ok(form, 'the real lookup must draw the offer form');
  const choice = form.querySelector('input[name="call"]');
  assert.equal(choice.checked, false);
  choice.checked = allow;
  form.submit(); await settle();
  const sent = r.posts.find(p => p.path === '/api/agents');
  assert.ok(sent, 'the real submit must send the grant');
  assert.equal(sent.body.scopes.includes(setup), allow);
});
for (const keep of [false, true]) test(`renewal preserves the setup choice (${keep})`, async () => {
  const r = rig(true); await settle();
  r.all().find(e => e.tag === 'button' && e.textContent === 'Renew or narrow').click();
  const form = r.roots['access-rows'].all().find(e => e.tag === 'form');
  const choice = form.querySelector('input[name="call"]');
  assert.equal(choice.checked, true);
  choice.checked = keep;
  form.submit(); await settle();
  const sent = r.posts.find(p => p.path === '/api/agents/agent1/widen');
  assert.ok(sent);
  assert.equal(sent.body.scopes.includes(setup), keep);
});

for (const cloud of [{link:'off'}, {link:'on',statisticsOnly:true}, {link:'ending'}, {link:'on',cloudTermsAccepted:false}]) test(`Cloud readiness blocks code consumption: ${JSON.stringify(cloud)}`,async()=>{
 const r=rig(false,cloud);await settle();
 r.roots['access-code'].value='TESTCODE';r.roots['access-code-form'].submit();await settle();
 assert.equal(r.posts.some(p=>p.path==='/api/agents/offer'),false);
 assert.equal(r.roots['access-lookup'].disabled,true);
});
