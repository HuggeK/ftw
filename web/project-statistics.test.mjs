import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';
import {setImmediate} from 'node:timers/promises';

const source=readFileSync(new URL('./project-statistics.js',import.meta.url),'utf8');
function page(cloud=false,authorised=true) {
  class Element {
    constructor(tag){this.tag=tag;this.children=[];this.events={};this.ownText='';}
    appendChild(e){this.children.push(e);return e;}
    insertBefore(e){this.children.unshift(e);}
    set textContent(v){this.ownText=v;this.children=[];}
    get textContent(){return this.ownText+this.children.map(e=>e.textContent).join(' ');}
    setAttribute(){}
    addEventListener(k,f){this.events[k]=f;}
  }
  const main=new Element('main');
  const all=()=>{const out=[];const visit=e=>{out.push(e);e.children.forEach(visit);};visit(main);return out;};
  let state={link:cloud?'on':'off',statisticsDecisionRequired:true,statisticsOnly:false};
  const writes=[];
  const policy={policy:'stats-current',cloud_policy:'cloud-current',cloud_scopes:['ftw.live.read'],available:false,cloud_includes_statistics:true,standalone:true};
  const window={addEventListener(){},confirm:()=>true,fetch:async(path,req)=>{
    if(req){writes.push({path,...req});state={...state,statisticsDecisionRequired:false,statisticsActive:req.method==='POST'};}
    return {ok:authorised,json:async()=>path.endsWith('stats-policy')?policy:state};
  }};
  const document={readyState:'loading',addEventListener(){},createElement:t=>new Element(t),getElementById:id=>all().find(e=>e.id===id),querySelector:()=>main,querySelectorAll:()=>[]};
  vm.runInNewContext(source,{window,document,Promise,Error});
  return {window,writes,main,buttons:()=>all().filter(e=>e.tag==='button')};
}

test('local question sends nothing before a real Yes or No',async()=>{
  const p=page();await p.window.FTWStatistics.refresh();
  assert.equal(p.writes.length,0);
  assert.deepEqual(p.buttons().map(e=>e.textContent),['Yes, contribute','No, keep it local']);
  assert.equal(p.buttons()[0].className,'btn-send');
  p.buttons()[1].events.click();await setImmediate();
  assert.equal(p.writes.length,1);assert.equal(p.writes[0].method,'DELETE');
  assert.equal(p.writes[0].path,'/api/cloud/stats');
  assert.match(p.main.textContent,/sharing is off/);
});

test('local Yes grants only statistics, not Cloud access',async()=>{
  const p=page();await p.window.FTWStatistics.refresh();p.buttons()[0].events.click();await setImmediate();
  assert.equal(p.writes.length,1);assert.equal(p.writes[0].path,'/api/cloud/stats');
  assert.deepEqual(JSON.parse(p.writes[0].body),{policy:'stats-current'});
});

test('Cloud has bundled terms and a whole-service off action',async()=>{
  const p=page(true);await p.window.FTWStatistics.refresh();
  assert.deepEqual(p.buttons().map(e=>e.textContent),['Accept Cloud terms','Turn FTW Cloud off']);
  assert.match(p.main.textContent,/Everyone using FTW Cloud contributes/);
  p.buttons()[0].events.click();await setImmediate();
  assert.equal(p.writes.length,1);assert.equal(p.writes[0].path,'/api/cloud/grant');
  assert.equal(JSON.parse(p.writes[0].body).policy,'cloud-current');
});

test('an unauthenticated page neither asks nor writes on behalf of an owner',async()=>{
  const p=page(false,false);await p.window.FTWStatistics.refresh();
  assert.equal(p.buttons().length,0);assert.equal(p.writes.length,0);
});
