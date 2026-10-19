import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';
import vm from 'node:vm';
const source=readFileSync(new URL('./setup-vendor.js',import.meta.url),'utf8');
function page(response){
 const input={value:'https://registered.example/callback?code=closed-fixture&state=closed-fixture'};
 const button={disabled:false};const note={textContent:''};let submit;
 const form={addEventListener(name,fn){submit=fn;},querySelector(){return button;}};
 const document={getElementById(id){return {finish:form,redirect:input,result:note}[id];}};
 vm.runInNewContext(source,{document,fetch:async()=>response,Error});
 return {input,note,button,submit:()=>submit({preventDefault(){}})};
}
test('a refused local sign-in retains the vendor redirect for retry',async()=>{
 const p=page({ok:false,json:async()=>({error:'valid LAN password required'})});
 const before=p.input.value;await p.submit();
 assert.equal(p.input.value,before);assert.equal(p.button.disabled,false);
 assert.match(p.note.textContent,/valid LAN password required/);
});
test('successful vendor consent clears the address and identifies draft state',async()=>{
 const p=page({ok:true,json:async()=>({status:'saved_to_draft',applied:false})});await p.submit();
 assert.equal(p.input.value,'');assert.match(p.note.textContent,/not running yet/);
 assert.equal(p.button.disabled,false);
});
