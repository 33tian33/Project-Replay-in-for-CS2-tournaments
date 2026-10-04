const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const script=fs.readFileSync('internal/replay/web/output.js','utf8');
async function fixture(block=false,clipsOnly=false){
 let server={id:'session',index:0,due:0,items:[{id:'intro',kind:'transition'},{id:'clip1',kind:'replay'},{id:'clip2',kind:'replay'},{id:'outro',kind:'transition'}]};
 if(clipsOnly)server.items=server.items.filter(item=>item.kind==='replay');
 const label={hidden:true,className:'',addEventListener(name,fn){this[name]=fn}},acks=[];
 function video(){return {style:{display:'none'},paused:true,src:'',duration:2,currentTime:0,loads:0,load(){this.loads++},pause(){this.paused=true},removeAttribute(){this.src=''},play(){if(block)return Promise.reject(Object.assign(Error('blocked'),{name:'NotAllowedError'}));this.paused=false;queueMicrotask(()=>this.onplaying?.());return Promise.resolve()}}}
 const videos=[video(),video()];let time=10000,offline=false,watchdog;
 const context=vm.createContext({document:{querySelector:s=>s==='#program'?videos[0]:s==='#standby'?videos[1]:label},location:{hash:''},crypto:{randomUUID:()=> 'obs'},URLSearchParams,AbortController,Date:{now:()=>time},requestAnimationFrame:()=>1,setTimeout:()=>1,clearTimeout(){},setInterval:fn=>{watchdog=fn},fetch:async(url,options)=>{if(offline)throw Error('offline');if(url.includes('/ack?')){const ack=JSON.parse(options.body);acks.push(ack);if(ack.error||++server.index===server.items.length)server={};return {ok:true,json:async()=>({ok:true})}}return {ok:true,json:async()=>structuredClone(server)}}});
 vm.runInContext(script,context);await new Promise(setImmediate);
 return {videos,label,acks,append(){server.items.splice(server.items.length-1,0,{id:"late",kind:"replay"})},async next(){const current=videos.find(v=>!v.paused);assert.ok(current);current.onended();await context.poll();await new Promise(setImmediate)},async poll(){await context.poll();await new Promise(setImmediate)},disconnect(){offline=true;time+=4000;watchdog()},stop(){server={}}};
}
test('transition → two replay clips → transition → transparent, with preloading',async()=>{
 const f=await fixture();assert.equal(f.label.hidden,true);assert.ok(f.videos.some(v=>v.src.includes('clip1')));
 await f.next();assert.equal(f.label.hidden,false);assert.ok(f.videos.some(v=>v.src.includes('clip2')));
 await f.next();assert.equal(f.label.hidden,false);
 await f.next();assert.equal(f.label.hidden,true);
 await f.next();assert.ok(f.videos.every(v=>v.paused&&v.style.display==='none'));assert.deepEqual(f.acks.map(a=>a.index),[0,1,2,3]);
});
test('autoplay error is acknowledged and restores transparency',async()=>{const f=await fixture(true);await f.poll();assert.equal(f.acks[0].error,'autoplay');assert.equal(f.label.hidden,true);assert.ok(f.videos.every(v=>v.style.display==='none'))});
test('lost heartbeat hides both video and Replay without a server response',async()=>{const f=await fixture();await f.next();f.disconnect();assert.equal(f.label.hidden,true);assert.ok(f.videos.every(v=>v.paused&&v.style.display==='none'))});
test('manual stop returns to transparent output',async()=>{const f=await fixture();await f.next();f.stop();await f.poll();assert.equal(f.label.hidden,true);assert.ok(f.videos.every(v=>v.style.display==='none'))});

test('tag enters once, holds between clips, and exits before final clip ends',async()=>{
 const f=await fixture();await f.next();assert.equal(f.label.className,'entering');
 f.label.animationend({animationName:'tag-in'});assert.equal(f.label.className,'');
 let v=f.videos.find(v=>!v.paused);v.currentTime=1.8;v.ontimeupdate();assert.equal(f.label.className,'');
 await f.next();assert.equal(f.label.className,'');assert.equal(f.label.hidden,false);
 v=f.videos.find(v=>!v.paused);v.currentTime=1.5;v.ontimeupdate();assert.equal(f.label.className,'');
 v.currentTime=1.61;v.ontimeupdate();assert.equal(f.label.className,'exiting');
 f.label.animationend({animationName:'tag-out'});assert.equal(f.label.hidden,true);
});

test('no imported transitions still shows the tag and exits on the last replay',async()=>{
 const f=await fixture(false,true);assert.equal(f.label.hidden,false);
 f.label.animationend({animationName:'tag-in'});await f.next();assert.equal(f.label.className,'');
 const v=f.videos.find(v=>!v.paused);v.currentTime=1.7;v.ontimeupdate();assert.equal(f.label.className,'exiting');
 await f.next();assert.equal(f.label.hidden,true);assert.ok(f.videos.every(v=>v.style.display==='none'));
});

test('late replay replaces preloaded outro without playing the wrong video',async()=>{
 const f=await fixture();await f.next();await f.next();
 f.append();await f.poll();await f.next();
 const current=f.videos.find(v=>!v.paused);
 assert.equal(current.src,'/output-api/media/late');
 await f.next();assert.equal(f.videos.find(v=>!v.paused).src,'/output-api/media/outro');
});

// Timers are deliberately inert in this fixture: ended must advance on its own.
test('ended immediately advances without waiting for the heartbeat timer',async()=>{
 const f=await fixture(false,true);
 f.videos.find(v=>!v.paused).onended();
 await new Promise(setImmediate);
 assert.deepEqual(f.acks.map(a=>a.index),[0]);
 assert.equal(f.videos.find(v=>!v.paused).src,'/output-api/media/clip2');
});

test('late replay is preloaded while the current clip is still playing',async()=>{
 const f=await fixture();await f.next();await f.next();
 const current=f.videos.find(v=>!v.paused);
 f.append();await f.poll();
 assert.equal(f.videos.find(v=>v!==current).src,'/output-api/media/late');
 assert.equal(current.paused,false);
 const loads=f.videos.map(v=>v.loads);
 await f.poll();await f.poll();
 assert.deepEqual(f.videos.map(v=>v.loads),loads);
});

test('simultaneous poll requests do not acknowledge a clip twice',async()=>{
 const f=await fixture(false,true);
 f.videos.find(v=>!v.paused).onended();
 await Promise.all([f.poll(),f.poll(),f.poll()]);
 assert.deepEqual(f.acks.map(a=>a.index),[0]);
 assert.equal(f.videos.find(v=>!v.paused).src,'/output-api/media/clip2');
});
