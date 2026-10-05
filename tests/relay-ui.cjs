// Optional packaged-app browser smoke test. Uses temporary data, synthetic
// group codes, loopback sockets and a temporary GSI cfg directory only.
const { _electron, chromium } = require(process.env.PLAYWRIGHT_MODULE || '@playwright/test');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const {spawn} = require('node:child_process');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '..');
const arch = process.arch === 'arm64' ? 'arm64' : 'amd64';
const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'replay-relay-ui-'));
const processes=[];
async function port(){const s=net.createServer();await new Promise(r=>s.listen(0,'127.0.0.1',r));const p=s.address().port;await new Promise(r=>s.close(r));return p}
async function start(exe,args,p){const child=spawn(exe,args,{stdio:['ignore','pipe','pipe']});processes.push(child);let logs='';child.stdout.on('data',b=>logs+=b);child.stderr.on('data',b=>logs+=b);for(let i=0;i<100;i++){if(child.exitCode!==null)throw Error('test process stopped: '+logs);try{const r=await fetch(`http://127.0.0.1:${p}/healthz`);if(r.status===200||r.status===404)return}catch{}await new Promise(r=>setTimeout(r,50))}throw Error('server startup timed out')}
async function main(){
 let browser;
 try{
  const cp=await port(),dp=await port(),ap=await port();
  const creds=[{role:'director'},{role:'agent'}];
  fs.mkdirSync(path.join(temp,'cfg'));
  await start(path.join(root,`dist/ProjectReplay-Relay-0.2.6-linux-${arch}/replay-relay`),['-listen',`127.0.0.1:${cp}`,'-data',path.join(temp,'cloud')],cp);
  const exe=path.join(root,`dist/ProjectReplay-0.2.6-linux-${arch}/project-replay`);
  await start(exe,['-role','director','-listen',`127.0.0.1:${dp}`,'-data',path.join(temp,'director'),'-no-browser'],dp);
  await start(exe,['-role','agent','-listen',`127.0.0.1:${ap}`,'-data',path.join(temp,'agent'),'-cs2-cfg',path.join(temp,'cfg'),'-no-browser'],ap);
  const app=path.join(temp,'electron.cjs');fs.writeFileSync(app,"const {app,BrowserWindow}=require('electron');app.whenReady().then(()=>new BrowserWindow({width:1440,height:1100,show:false}).loadURL('about:blank'));\n");
  console.log('Fixture ready; launching browser');
  let director,agent;
  if(process.env.CHROMIUM_PATH){browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH,headless:true,args:['--no-sandbox']});const context=await browser.newContext({viewport:{width:1440,height:1100}});director=await context.newPage();agent=await context.newPage();}
  else {browser=await _electron.launch({timeout:15000,executablePath:process.env.ELECTRON_PATH,args:['--no-sandbox',app],env:process.env});director=await browser.firstWindow({timeout:15000});console.log('First window ready');const waiting=browser.waitForEvent('window',{timeout:15000});await browser.evaluate(({BrowserWindow})=>{global.testWindow=new BrowserWindow({width:1440,height:1100,show:false});global.testWindow.loadURL('about:blank')});agent=await waiting;}
  console.log('Both windows ready');director.setDefaultTimeout(15000);agent.setDefaultTimeout(15000);
  const errors=[];for(const p of [director,agent])p.on('pageerror',e=>errors.push(e.message));
  await director.goto(`http://127.0.0.1:${dp}`);await agent.goto(`http://127.0.0.1:${ap}`);
  for(const [page,cred] of [[director,creds[0]],[agent,creds[1]]]){
   console.log('Configuring '+cred.role);await page.locator('#settings-btn').click();
   assert.equal(await page.locator('[name=connection_mode]').inputValue(),'lan');
   await page.locator('[name=connection_mode]').selectOption('relay');
   await page.locator('[name=relay_url]').fill(`http://127.0.0.1:${cp}`);
   assert.equal(await page.locator('[name=relay_device],[name=relay_token]').count(),0);
   await page.locator('[name=relay_group]').fill('0001');
   await page.locator('[name=relay_name]').fill(cred.role==='director'?'测试主机':'测试录制机');
   await page.locator('#settings-form button[type=submit]').click();
   await page.locator('#settings').waitFor({state:'hidden'});
   await page.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('已连接'));
  }
  console.log('Pairing manually');await director.locator('#relay-search').click();await director.locator('[data-relay-pair]').click();
  await agent.locator('#relay-accept').waitFor({state:'visible'});await agent.locator('#relay-accept').click();
  await director.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('已配对'));
  await director.locator('#relay-unpair').click();await agent.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('等待同组设备'));
  await agent.locator('#relay-auto').check();
  await director.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('已配对'));
  await agent.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('对端在线'));
  await director.screenshot({path:path.join(temp,'director.png'),fullPage:true});await agent.screenshot({path:path.join(temp,'agent.png'),fullPage:true});
  await agent.locator('#settings-btn').click();assert.equal(await agent.locator('[name=relay_group]').inputValue(),'0001');assert.equal(await agent.locator('[name=relay_token]').count(),0);await agent.locator('[name=relay_group]').fill('0002');await agent.locator('#settings-form button[type=submit]').click();await agent.locator('#settings').waitFor({state:'hidden'});
  await director.waitForFunction(()=>document.querySelector('#relay-status').textContent.includes('等待同组设备'));
  await director.locator('#relay-search').click();await director.waitForFunction(()=>document.querySelector('#relay-nodes').textContent.includes('暂无在线的同组设备'));
  assert.deepEqual(errors,[]);
  console.log(JSON.stringify({ok:true,checks:['LAN default','group leading zero','manual pairing','automatic pairing','group isolation','no token or device ID inputs','no page errors'],evidence:temp}));
 }finally{
  if(browser)await browser.close();
  for(const child of processes)child.kill('SIGTERM');
  await Promise.all(processes.map(child=>child.exitCode!==null?Promise.resolve():new Promise(resolve=>child.once('exit',resolve))));
 }
}
main().catch(e=>{console.error(e);process.exitCode=1});
