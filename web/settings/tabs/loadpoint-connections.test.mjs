import assert from 'node:assert/strict';
import { describe, it } from 'node:test';
globalThis.window = {};
await import('./loadpoints.js');
const S = window.FTWSettings;
const escHtml = s => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
const render = config => S.tabs.loadpoints.render({config, escHtml, help: () => ''});
const config = () => ({
 drivers: [{name: 'charger-current', lua: 'drivers/charger.lua'}],
 loadpoints: [{id: 'garage', driver_name: 'charger-old'}]
});

describe('saved charger connections', () => {
 it('shows the missing saved connection without selecting a replacement', () => {
  S.catalogByLua = {'drivers/charger.lua': {capabilities: ['ev']}};
  S.ocppStatus = {enabled: false, chargers: []};
  const cfg = config(), html = render(cfg);
  assert.match(html, /charger-old \(missing connection\)/);
  assert.match(html, /data-charger-connection-status="0" role="alert"/);
  assert.match(html, /Select a configured charger or remove this charger/);
  assert.equal(cfg.loadpoints[0].driver_name, 'charger-old');
  const savedChoice = html.match(/<select data-path="loadpoints\.0\.driver_name">([\s\S]*?)<\/select>/)[1];
  assert.doesNotMatch(savedChoice, /value="charger-current" selected/);
  cfg.loadpoints[0].driver_name = 'charger-current';
  const repaired = render(cfg);
  assert.doesNotMatch(repaired, /missing connection|data-charger-connection-status/);
 });
 it('does not treat an unloaded driver catalogue as a missing configured driver', () => {
  S.catalogByLua = null;
  const cfg = config(); cfg.loadpoints[0].driver_name = 'charger-current';
  assert.doesNotMatch(render(cfg), /missing connection|data-charger-connection-status/);
 });
 it('keeps a possible offline OCPP identity as unverified', () => {
  const cfg = {drivers: [], ocpp: {enabled: true}, loadpoints: [{id: 'garage', driver_name: 'ocpp-garage'}]};
  S.ocppStatus = {enabled: true, chargers: []};
  const html = render(cfg);
  assert.match(html, /ocpp-garage \(OCPP not seen\)/);
  assert.match(html, /role="status"/);
  assert.doesNotMatch(html, /missing connection/);
  assert.equal(cfg.loadpoints[0].driver_name, 'ocpp-garage');
 });
 it('escapes an old connection name in its warning and option', () => {
  S.ocppStatus = {enabled: false, chargers: []};
  const cfg = config(); cfg.loadpoints[0].driver_name = '<broken>';
  const html = render(cfg);
  assert.doesNotMatch(html, /<broken>/);
  assert.match(html, /&lt;broken&gt;/);
 });
});
