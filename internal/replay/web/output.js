'use strict';
const client=crypto.randomUUID?crypto.randomUUID():String(Date.now())+Math.random().toString(36).slice(2);
let video=document.querySelector('#program'),standby=document.querySelector('#standby');
const label=document.querySelector('#replay-label');
let tagSession='';
label.addEventListener('animationend',e=>{if(e.animationName==='tag-in')label.className='';if(e.animationName==='tag-out')label.hidden=true});
function hideTag(){label.hidden=true;label.className='';tagSession=''}
function showTag(session){if(tagSession===session)return;tagSession=session;label.hidden=false;label.className='entering'}
function exitTag(){if(!label.hidden&&label.className!=='exiting')label.className='exiting'}
let playing='',lastOK=0,active=null,ackPending=null,loadingSince=0,preloaded='',pollTimer=null,polling=false,pollAgain=false;
function reset(v){v.ontimeupdate=null;v.onended=null;v.onerror=null;v.onplaying=null;v.pause();v.removeAttribute('src');v.load();v.style.display='none'}
function clear(){reset(video);reset(standby);hideTag();playing='';active=null;loadingSince=0;preloaded=''}
function source(item){return '/output-api/media/'+encodeURIComponent(item.id)}
async function call(path,body){const controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),2000);try{const r=await fetch('/output-api/'+path+'?client='+encodeURIComponent(client),{method:body?'POST':'GET',headers:body?{'Content-Type':'application/json'}:undefined,body:body?JSON.stringify(body):undefined,signal:controller.signal});if(!r.ok){const error=Error(String(r.status));error.status=r.status;throw error}return await r.json()}finally{clearTimeout(timeout)}}
function preloadNext(s){const next=s.items[s.index+1],identity=next?s.id+':'+(s.index+1)+':'+next.id:'';if(preloaded===identity)return;reset(standby);preloaded=identity;if(next){standby.src=source(next);standby.load()}}
function finish(error=''){if(!active||ackPending)return;ackPending={id:active.id,index:active.index,client,error};video.pause();if(error){video.style.display='none';hideTag()}loadingSince=0;void poll()}
async function poll(){if(polling){pollAgain=true;return}clearTimeout(pollTimer);polling=true;try{
 if(ackPending){try{await call('ack',ackPending)}catch(e){if(e.status!==409)throw e}ackPending=null}
 const s=await call('state');lastOK=Date.now();
 if(!s.id||s.due>0||!s.items?.[s.index]){if(playing)clear();return}
 const item=s.items[s.index],identity=s.id+':'+s.index+':'+item.id;
 active=s;
 if(playing!==identity){
  const oldVideo=video;[video,standby]=[standby,video];
  playing=identity;active=s;loadingSince=Date.now();
  video.ontimeupdate=()=>{if(playing===identity&&item.kind==='replay'&&active?.items[active.index+1]?.kind!=='replay'&&Number.isFinite(video.duration)&&video.duration-video.currentTime<=.4)exitTag()};
  video.onended=()=>{if(playing===identity)finish()};video.onerror=()=>{if(playing===identity)finish('decode')};
  video.onplaying=()=>{if(playing!==identity)return;loadingSince=0;video.style.display='block';oldVideo.style.display='none';oldVideo.pause();if(item.kind==='replay')showTag(s.id);else hideTag();preloaded='';preloadNext(active)};
  if(preloaded!==identity)video.src=source(item);
  video.play().catch(e=>{if(playing===identity)finish(e.name==='NotAllowedError'?'autoplay':'decode')});
 }
 if(!loadingSince&&!ackPending)preloadNext(s);
 if(loadingSince && Date.now()-loadingSince>10000)finish('load timeout');
 }catch(e){if(e.status===409 || Date.now()-lastOK>2500){ackPending=null;clear()}}
 finally{polling=false;if(pollAgain){pollAgain=false;void poll()}else pollTimer=setTimeout(poll,200)}}
setInterval(()=>{if(Date.now()-lastOK>3000&&playing){ackPending=null;clear()}},250);
poll();

// Frame sampling avoids late exits from the browser's low-frequency timeupdate event.
function tagFrame(){if(active&&!video.paused)video.ontimeupdate?.();requestAnimationFrame(tagFrame)}
requestAnimationFrame(tagFrame);
