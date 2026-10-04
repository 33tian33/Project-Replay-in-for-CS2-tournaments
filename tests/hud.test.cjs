const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
test('HUD displays literal team names, updates logos and hides stale data',async()=>{
 const els=new Map();const el=s=>{if(!els.has(s))els.set(s,{hidden:false,textContent:'',src:'',getAttribute(){return this.src}});return els.get(s)};
 let state={visible:true,fresh:true,ct:{name:'<script>team</script>',logo:'data:image/png;base64,test'},t:{name:'T',logo:''},ct_score:7,t_score:4,round:12,clock:{remaining:73.2,phase:'live'}};
 let retry;
 const ctx=vm.createContext({document:{querySelector:el},fetch:async()=>({ok:true,json:async()=>state}),setTimeout:fn=>retry=fn});
 vm.runInContext(fs.readFileSync('internal/replay/web/hud.js','utf8'),ctx);await new Promise(setImmediate);
 assert.equal(el('#hud').hidden,false);assert.equal(el('#ct-name').textContent,'<script>team</script>');assert.equal(el('#t-logo').hidden,true);assert.equal(el('#clock').textContent,'1:14');
 state.fresh=false;await retry();assert.equal(el('#hud').hidden,true);
 state.fresh=true;state.visible=false;await retry();assert.equal(el('#hud').hidden,true);
 ctx.fetch=async()=>{throw Error('offline')};await retry();assert.equal(el('#hud').hidden,true);
});
