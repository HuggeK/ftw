import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

test('display smooths the supplied household balance and resets across gaps', () => {
  const app = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const source = app.slice(app.indexOf('  function smoothDisplayNumber('), app.indexOf('  function smoothSeriesForChart('));
  const c = {statusDisplayState:{}, STATUS_DISPLAY_TAU_MS:400, Date:{now:()=>1000}};
  vm.createContext(c); vm.runInContext(source,c);
  // V2X is already part of Core's raw residual. Re-deriving Home use
  // from separately smoothed display fields dropped it and broke balance.
  const status = {grid_w:2500, bat_w:0, pv_w:0, ev_w:0, v2x_w:1000, load_w:1500};
  assert.equal(c.smoothStatusForDisplay(status).load_w,1500);
  assert.equal(status.load_w,1500);
  assert.equal(c.smoothStatusForDisplay({...status,load_w:null}).load_w,null);
  assert.equal(c.smoothStatusForDisplay({...status,load_w:500}).load_w,500);
});
