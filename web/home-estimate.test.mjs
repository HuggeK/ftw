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

test('the rendered house marks a mean and never combines it with live self-powered share', () => {
  const module = readFileSync(new URL('./components/ftw-energy-flow.js', import.meta.url), 'utf8')
    .replace(/^import .+;$/m, '').replace(/^export \{[^\n]+\};$/m, '');
  let Flow;
  const c = {
    FtwElement: class {
      constructor() { this.style = {setProperty(){}}; }
      hasAttribute() { return false; }
      toggleAttribute() {}
      update() { this.html = this.render(); }
    },
    ftwDebugDelay: () => 0,
    document: {addEventListener(){}},
    performance: {now:()=>0},
    customElements: {define:(_, type)=>{Flow=type;}},
  };
  vm.createContext(c); vm.runInContext(module, c);
  const flow = new Flow();
  const planets = [{id:'grid',role:'grid',corner:'bottom-left',kw:1,toHub:true,title:'GRID'}];
  flow.setReadings({load:2,loadEstimated:true,planets,selfPoweredPctToday:70});
  assert.match(flow.html, /≈ 2.0/);
  assert.match(flow.html, /House load, estimated/);
  assert.doesNotMatch(flow.html, /SELF-POWERED NOW/);
  assert.match(flow.html, /70% SELF-POWERED TODAY/);
  flow.setReadings({load:2,loadEstimated:false,planets});
  assert.match(flow.html, /50% SELF-POWERED NOW/);
  flow.setReadings({load:null,loadEstimated:true,planets});
  assert.match(flow.html, /House load, no data/);
  assert.doesNotMatch(flow.html, /SELF-POWERED NOW/);
});
